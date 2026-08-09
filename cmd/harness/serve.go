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
	if cfg.WorkspaceRoot == "" {
		fmt.Fprintln(os.Stderr, "harness: warning: DEEPSEEK_WORKSPACE_ROOT is not set; every work request will fail before its session starts")
	}

	priceTable, err := pricing.Load(cfg.PriceTablePath)
	if err != nil {
		return err
	}

	client := deepseek.NewClient(cfg.BaseURL, cfg.APIKey)
	logStartupBalance(ctx, client)
	logStartupModels(ctx, client, cfg.Model, cfg.FlashModel)

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
		Client:     client,
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
		Store:            st,
		Runner:           runner,
		JS:               js,
		Consumer:         consumer,
		WorkspaceRoot:    cfg.WorkspaceRoot,
		DefaultModel:     cfg.Model,
		DefaultEffort:    cfg.Effort,
		DefaultThinking:  cfg.Thinking,
		DefaultMaxTokens: cfg.MaxTokens,
		DefaultDeadline:  time.Duration(cfg.DefaultDeadlineMS) * time.Millisecond,
		PriceTableDate:   priceTable.CapturedAt,
		Size:             cfg.WorkerPoolSize,
	}

	static, err := httpapi.NewStaticHandler(cfg.DevFrontendURL)
	if err != nil {
		return err
	}
	api := &httpapi.Server{
		Store: st, Hub: eventHub, Static: static,
		Consumer: consumer, Pool: pool, PriceTableDate: priceTable.CapturedAt,
	}
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

	log.Printf("harness serve: connected to %s, pool size %d, model %s (flash %s), workspace root %s",
		cfg.NATSURL, cfg.WorkerPoolSize, cfg.Model, cfg.FlashModel, cfg.WorkspaceRoot)
	log.Printf("harness serve: http listening on %s", cfg.HTTPAddr)
	return pool.Run(ctx)
}

// logStartupBalance refreshes the account balance once at startup
// (docs/DESIGN.md §4.5). It only logs: an empty account
// found here does not stop the pool from starting, because the reactive
// path (worker.Pool.Halt on an actual 402) is what docs/DESIGN.md means by
// "stops the pool rather than failing each queued request in turn" — a
// balance that looks fine now and runs out mid-run is exactly the case that
// path exists for, so there is no separate startup gate to duplicate it.
func logStartupBalance(ctx context.Context, client *deepseek.Client) {
	balCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bal, err := client.GetBalance(balCtx)
	if err != nil {
		log.Printf("harness serve: could not check account balance at startup: %v", err)
		return
	}
	if !bal.IsAvailable {
		log.Printf("harness serve: warning: account balance is empty (is_available=false); queued work will fail until it is topped up")
		return
	}
	for _, b := range bal.BalanceInfos {
		log.Printf("harness serve: balance available: %s %s (granted %s, topped up %s)",
			b.TotalBalance, b.Currency, b.GrantedBalance, b.ToppedUpBalance)
	}
}

// logStartupModels fetches the live model list once at startup and warns if
// the configured main or flash model is not on it — the sanity check
// docs/MODELS.md's "Populate the list from the API" describes, catching a
// typo or a model DeepSeek has retired before it costs a queued request a
// 400 instead. It only warns: GET /models failing, or a name it does not
// recognise, is not a reason to refuse to start.
func logStartupModels(ctx context.Context, client *deepseek.Client, model, flashModel string) {
	modelsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := client.ListModels(modelsCtx)
	if err != nil {
		log.Printf("harness serve: could not fetch the live model list at startup: %v", err)
		return
	}
	live := make(map[string]bool, len(resp.Data))
	for _, m := range resp.Data {
		live[m.ID] = true
	}
	for _, want := range []string{model, flashModel} {
		if !live[want] {
			log.Printf("harness serve: warning: configured model %q was not in GET /models' live list", want)
		}
	}
}
