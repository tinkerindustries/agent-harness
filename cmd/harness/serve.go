package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/httpapi"
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
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
	poolSize := fs.Int("pool-size", 0, "override worker pool size (default: the worker.pool_size setting)")
	addr := fs.String("addr", "", "override the HTTP address (default from config; loopback)")
	devFrontend := fs.String("dev-frontend", "", "proxy non-API requests to a running Vite dev server at this URL instead of serving the embedded build")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
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

	rec := newHTTPLogRecorder(cfg)
	defer closeHTTPLog(rec)

	// The store must be open before the client makes its first request:
	// the client reads the DeepSeek API key from the settings table on
	// every call, and serve must start (and serve) with no key stored — an
	// operator sets one afterwards, and runs fail individually until then.
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	res := settings.NewResolver(st)

	// The run-control bearer token: http.control_token, generated when the
	// setting is empty so an installation that has never had one gets one on
	// its first start (docs/RUN-CONTROL.md "Authentication"). 32 bytes of
	// crypto/rand, base64url, stored through the ordinary settings path like
	// any other key. The value is never logged; only the fact of generation
	// is. The process keeps its own copy on the HTTP server, so the endpoint
	// works even if the setting is later deleted — and a Server built without
	// this step (every test that does not set one) has an empty token and the
	// stop endpoint fails closed with 503.
	controlToken, err := res.String(ctx, settings.KeyHTTPControlToken)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyHTTPControlToken, err)
	}
	if controlToken == "" {
		controlToken, err = generateControlToken()
		if err != nil {
			return err
		}
		if err := res.Set(ctx, settings.KeyHTTPControlToken, controlToken); err != nil {
			return fmt.Errorf("store generated %s: %w", settings.KeyHTTPControlToken, err)
		}
		log.Printf("harness serve: generated a new %s (the run-control bearer token)", settings.KeyHTTPControlToken)
	}

	// Restart-required settings, resolved once at startup: the worker pool
	// size, the two model-concurrency ceilings, the RESULTS stream
	// retention, and the events paging bounds are read at startup or baked
	// into the JetStream stream, so a change takes effect on the next start
	// (the settings screen marks each of these; docs/DESIGN.md §4.2).
	workerPoolSize, err := res.Int(ctx, settings.KeyWorkerPoolSize)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyWorkerPoolSize, err)
	}
	if *poolSize != 0 {
		workerPoolSize = *poolSize
	}
	maxDeliveryAttempts, err := res.Int(ctx, settings.KeyWorkerMaxDeliveryAttempts)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyWorkerMaxDeliveryAttempts, err)
	}
	concurrencyPro, err := res.Int(ctx, settings.KeyWorkerConcurrencyPro)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyWorkerConcurrencyPro, err)
	}
	concurrencyFlash, err := res.Int(ctx, settings.KeyWorkerConcurrencyFlash)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyWorkerConcurrencyFlash, err)
	}
	resultsMaxAge, err := res.Duration(ctx, settings.KeyQueueResultsMaxAge)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyQueueResultsMaxAge, err)
	}
	eventsLimitDefault, err := res.Int(ctx, settings.KeyHTTPEventsLimitDefault)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyHTTPEventsLimitDefault, err)
	}
	eventsLimitMax, err := res.Int(ctx, settings.KeyHTTPEventsLimitMax)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyHTTPEventsLimitMax, err)
	}
	// The model names are live settings too; the startup warning and the
	// concurrency-limits map keyed by them snapshot today's values.
	defaultModel, err := res.String(ctx, settings.KeyDefaultModel)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyDefaultModel, err)
	}
	defaultFlashModel, err := res.String(ctx, settings.KeyDefaultFlashModel)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", settings.KeyDefaultFlashModel, err)
	}

	client := withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(res))
	logStartupBalance(ctx, client)
	logStartupModels(ctx, client, defaultModel, defaultFlashModel)

	eventHub := hub.New()
	runner := &session.Runner{
		Store:       st,
		Mirror:      store.NewMirror(cfg.DataDir),
		Client:      client,
		Recorder:    rec,
		Prices:      priceTable,
		Gemini:      withGeminiHTTPLog(cfg, rec, googleAPIKeyProvider(res)),
		GeminiModel: googleVisionModelProvider(res),
		Hub:         eventHub,
		Settings:    res,
		ModelLimits: map[string]int{
			defaultModel:      concurrencyPro,
			defaultFlashModel: concurrencyFlash,
		},
	}

	nc, js, err := queue.Connect(cfg.NATSURL)
	if err != nil {
		return err
	}
	defer nc.Close()

	ensureCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	consumer, err := queue.EnsureStreams(ensureCtx, js, workerPoolSize, resultsMaxAge, maxDeliveryAttempts)
	cancel()
	if err != nil {
		return fmt.Errorf("declare streams: %w", err)
	}

	pool := &worker.Pool{
		Store:               st,
		Runner:              runner,
		JS:                  js,
		Consumer:            consumer,
		WorkspaceRoot:       cfg.WorkspaceRoot,
		DefaultThinking:     cfg.Thinking,
		PriceTableDate:      priceTable.CapturedAt,
		Size:                workerPoolSize,
		MaxDeliveryAttempts: maxDeliveryAttempts,
		Settings:            res,
	}

	static, err := httpapi.NewStaticHandler(cfg.DevFrontendURL)
	if err != nil {
		return err
	}
	api := &httpapi.Server{
		Store: st, Hub: eventHub, Static: static, Settings: res,
		Consumer: consumer, Pool: pool, PriceTableDate: priceTable.CapturedAt,
		Run: pool, ControlToken: controlToken,
		DefaultEventsLimit: eventsLimitDefault,
		MaxEventsLimit:     eventsLimitMax,
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
		cfg.NATSURL, workerPoolSize, defaultModel, defaultFlashModel, cfg.WorkspaceRoot)
	log.Printf("harness serve: http listening on %s", cfg.HTTPAddr)
	return pool.Run(ctx)
}

// generateControlToken returns a fresh http.control_token value: 32 bytes of
// crypto/rand encoded as base64url without padding (docs/RUN-CONTROL.md
// "Authentication"). It never logs or returns the value in a way that names
// it; the caller stores it through the settings path and hands it to the HTTP
// server, and the startup log says only that one was generated.
func generateControlToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate %s: %w", settings.KeyHTTPControlToken, err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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
