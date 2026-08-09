package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/httpapi"
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/worker"
)

// runServe starts the harness as a service: a durable pull consumer on the
// WORK stream, a worker pool sized from config, and results published back
// to the RESULTS stream (docs/DESIGN.md §4.10). It runs until ctx is
// cancelled (SIGINT/SIGTERM, wired in main), draining in-flight runs rather
// than cutting them off.
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	poolSize := fs.Int("pool-size", 0, "override worker pool size (default from config)")
	addr := fs.String("addr", "", "override the HTTP address (default from config; loopback)")
	devFrontend := fs.String("dev-frontend", "", "proxy non-API requests to a running Vite dev server at this URL instead of serving the embedded build")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *poolSize != 0 {
		cfg.WorkerPoolSize = *poolSize
	}
	if *addr != "" {
		cfg.HTTPAddr = *addr
	}
	if *devFrontend != "" {
		cfg.DevFrontendURL = *devFrontend
	}
	if len(cfg.WorkspaceRoots) == 0 {
		fmt.Fprintln(os.Stderr, "harness: warning: DEEPSEEK_WORKSPACE_ROOTS is not set; every work request will be denied at validation")
	}

	priceTable, err := pricing.Load(cfg.PriceTablePath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	eventHub := hub.New()
	runner := &session.Runner{
		Store:      st,
		Mirror:     store.NewMirror(cfg.DataDir),
		Client:     deepseek.NewClient(cfg.BaseURL, cfg.APIKey),
		Prices:     priceTable,
		FlashModel: cfg.FlashModel,
		Hub:        eventHub,
		ModelLimits: map[string]int{
			cfg.Model:      cfg.ModelConcurrencyPro,
			cfg.FlashModel: cfg.ModelConcurrencyFlash,
		},
	}

	nc, js, err := queue.Connect(cfg.NATSURL)
	if err != nil {
		return err
	}
	defer nc.Close()

	ensureCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	consumer, err := queue.EnsureStreams(ensureCtx, js, cfg.WorkerPoolSize)
	cancel()
	if err != nil {
		return fmt.Errorf("declare streams: %w", err)
	}

	pool := &worker.Pool{
		Store:                 st,
		Runner:                runner,
		JS:                    js,
		Consumer:              consumer,
		Roots:                 cfg.WorkspaceRoots,
		DefaultModel:          cfg.Model,
		DefaultEffort:         cfg.Effort,
		DefaultThinking:       cfg.Thinking,
		DefaultMaxTokens:      cfg.MaxTokens,
		DefaultPermissionMode: tools.Mode(cfg.PermissionMode),
		DefaultDeadline:       time.Duration(cfg.DefaultDeadlineMS) * time.Millisecond,
		PriceTableDate:        priceTable.CapturedAt,
		Size:                  cfg.WorkerPoolSize,
	}

	static, err := httpapi.NewStaticHandler(cfg.DevFrontendURL)
	if err != nil {
		return err
	}
	api := &httpapi.Server{Store: st, Hub: eventHub, Static: static}
	httpSrv := &http.Server{Addr: cfg.HTTPAddr, Handler: api.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("harness serve: http shutdown: %v", err)
		}
	}()
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("harness serve: http server error: %v", err)
		}
	}()

	log.Printf("harness serve: connected to %s, pool size %d, model %s (flash %s), roots %v",
		cfg.NATSURL, cfg.WorkerPoolSize, cfg.Model, cfg.FlashModel, cfg.WorkspaceRoots)
	log.Printf("harness serve: http listening on %s", cfg.HTTPAddr)
	return pool.Run(ctx)
}
