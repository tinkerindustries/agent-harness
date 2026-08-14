package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/assets"
	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/evals"
	"github.com/mrgeoffrich/deepseek-harness/internal/httpapi"
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/kimi"
	harnessmcp "github.com/mrgeoffrich/deepseek-harness/internal/mcp"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/worker"
)

// runServe starts the harness as a service: a durable pull consumer on the
// WORK stream, a worker pool sized from config, and results published back
// to the RESULTS stream (docs/DESIGN.md §4.10). One process serves the web
// UI, the /api/... HTTP API, and the MCP launch server at /mcp on the same
// *http.Server. It runs until ctx is cancelled (SIGINT/SIGTERM, wired in
// main), draining in-flight runs rather than cutting them off.
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
	mcpCfg, err := config.LoadMCP()
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
	// The embedded MCP service gets the same token serve itself holds, so
	// deepseek_stop / deepseek_steer authenticate without an HTTP round-trip
	// to GET /api/control-token (that loopback fetch stays as defensive code
	// in internal/mcp/control.go for a Service built without this step).
	mcpCfg.ControlToken = controlToken

	// The operator name, when unset: a bare-metal `harness serve` names
	// itself from the login user, so runs started from the web UI record who
	// started them without a deliberate `harness config set
	// identity.operator geoff`. Inside Docker serve runs as root, where
	// os/user is useless, so this never fires there. Any failure is logged
	// and ignored — a convenience, never a startup failure (D7).
	if err := selfNameOperator(ctx, res); err != nil {
		log.Printf("harness serve: self-name identity.operator: %v", err)
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

	deepSeekClient := withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(res))
	kimiClient := withKimiHTTPLog(cfg, rec, kimiAPIKeyProvider(res))
	// The pool serves whichever model a request names, so the Runner routes
	// by model through a resolver built here — the one place both provider
	// clients exist (docs/KIMI-INTEGRATION.md §4.3).
	clientFor := func(model string) session.Client {
		return clientForModel(model, deepSeekClient, kimiClient)
	}
	switch providerFor(defaultModel) {
	case provider.Kimi:
		logStartupKimiBalance(ctx, kimiClient)
		logStartupKimiModels(ctx, kimiClient, defaultModel)
	default:
		logStartupBalance(ctx, deepSeekClient)
		logStartupModels(ctx, deepSeekClient, defaultModel, defaultFlashModel)
	}

	eventHub := hub.New()
	runner := &session.Runner{
		Store:       st,
		Mirror:      store.NewMirror(cfg.DataDir),
		Client:      deepSeekClient,
		ClientFor:   clientFor,
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

	// The MCP service reaches the harness's own API over loopback HTTP (the
	// documented internal/mcp → hub/store boundary; it opens no SQLite
	// handle). With no DEEPSEEK_HARNESS_BASE_URL set, derive it from the
	// address serve is about to bind — correct in every deployment, because
	// the MCP mount now lives on this very server rather than a second
	// process a compose file had to point at by service name.
	if os.Getenv("DEEPSEEK_HARNESS_BASE_URL") == "" {
		if _, port, err := net.SplitHostPort(cfg.HTTPAddr); err == nil {
			mcpCfg.HarnessBaseURL = "http://127.0.0.1:" + port
		}
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
		// The skills this build ships. Composition is the only place that
		// names them, so a deployment that wants none drops this line rather
		// than editing the pool (internal/skills.Install, assets/embed.go).
		SkillsFS: assets.AgentSkills(),
	}

	static, err := httpapi.NewStaticHandler(cfg.DevFrontendURL)
	if err != nil {
		return err
	}
	// The orchestrator runs evals inside this process, so a run survives the
	// terminal that started it and the browser can start one. It publishes
	// through the same one-method seam the browser's start uses; httpapi
	// still holds no JetStream handle (docs/EVALS.md).
	orchestrator := &evals.Orchestrator{
		Publisher: evalPublisher{js: js},
		Store:     st,
		OnChange:  eventHub.PublishEvalChanged,
		NewJudge: func(model string) (*evals.Judge, error) {
			return newJudge(ctx, res, model, deepSeekClient, kimiClient)
		},
	}

	api := &httpapi.Server{
		Store: st, Hub: eventHub, Static: static, Settings: res,
		Consumer: consumer, Pool: pool, PriceTableDate: priceTable.CapturedAt, Prices: priceTable,
		Run: pool, Publisher: publishAdapter{js: js}, ControlToken: controlToken,
		Evals:              evalControl{o: orchestrator},
		DefaultEventsLimit: eventsLimitDefault,
		MaxEventsLimit:     eventsLimitMax,
	}
	// The MCP launch server mounts on the same *http.Server as /api/... and
	// the web UI: one process, one port. It reuses serve's own JetStream
	// handle, control token, settings resolver, and store — the launch tool
	// writes its attachments through the store's single writer, never
	// opening a SQLite handle of its own (ARCHITECTURE.md) — and keeps
	// calling /api/... over loopback HTTP for the reads. The outer mux
	// lives here in cmd/, not inside internal/httpapi, because api.Handler()
	// is methodGate(s.routes()) and its allowlist would 405 a path it has
	// not been taught.
	mcpSvc := &harnessmcp.Service{
		JS:         js,
		Cfg:        mcpCfg,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Registry:   harnessmcp.NewRegistry(),
		Store:      st,
		Settings:   res,
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSvc.Handler())
	mux.Handle("/", api.Handler())
	httpSrv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}
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
	log.Printf("harness serve: MCP launch server mounted at %s/mcp (permission ceiling %s)",
		cfg.HTTPAddr, mcpCfg.PermissionCeiling)
	return pool.Run(ctx)
}

// newJudge builds the judge for one eval run: the model is the one the run
// names, or model.judge when it names none, and the client is resolved
// through the same model→provider table the runner uses (internal/provider,
// docs/KIMI-INTEGRATION.md §4.3) — so a kimi-k3 judge speaks to the Kimi
// client and a deepseek-v4-pro judge to the DeepSeek one. An unknown judge
// model is an error here rather than a fallback, unlike clientForModel's
// DeepSeek default: a judge that silently scored with the wrong provider's
// account — or silently vanished from the eval — would cost money and say
// nothing about the run.
func newJudge(ctx context.Context, res *settings.Resolver, model string, deepSeekClient *deepseek.Client, kimiClient *kimi.Client) (*evals.Judge, error) {
	if model == "" {
		resolved, err := res.String(ctx, settings.KeyJudgeModel)
		if err != nil {
			return nil, fmt.Errorf("evals: resolve judge model: %w", err)
		}
		model = resolved
	}
	p, err := provider.ModelFor(model)
	if err != nil {
		return nil, fmt.Errorf("evals: judge model: %w", err)
	}
	var client evals.Client = deepSeekClient
	if p == provider.Kimi {
		client = kimiClient
	}
	return &evals.Judge{Client: client, Model: model}, nil
}

// publishAdapter is the RunPublisher implementation for harness serve: the
// browser's POST /api/runs enqueues through the same JetStream handle the
// pool reads, so a browser-started run is byte-identical in the store to one
// started from MCP or the CLI — same event kinds, same validation, same
// idempotency on a duplicate request_id (docs/RUN-CONTROL.md "Starting is a
// publish, so the seam is a publisher"). The interface is declared in
// internal/httpapi and implemented here, in cmd/, because composition
// happens in cmd/ and nowhere else (ARCHITECTURE.md).
type publishAdapter struct {
	js jetstream.JetStream
}

// evalPublisher is the same publish, in the shape internal/evals declares.
// Two one-method interfaces over one handle rather than one shared interface,
// so neither package imports the other's vocabulary.
type evalPublisher struct {
	js jetstream.JetStream
}

func (a evalPublisher) Publish(ctx context.Context, req queue.Request) error {
	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return queue.PublishRequest(pubCtx, a.js, req)
}

// evalControl is the EvalController implementation: the orchestrator, named
// the way internal/httpapi asks for it.
type evalControl struct {
	o *evals.Orchestrator
}

func (c evalControl) StartEval(ctx context.Context, spec evals.Spec) (string, error) {
	return c.o.Start(ctx, spec)
}

func (c evalControl) CancelEval(evalRunID string) error { return c.o.Cancel(evalRunID) }

func (c evalControl) RunningEval(evalRunID string) bool { return c.o.Running(evalRunID) }

func (a publishAdapter) PublishRequest(ctx context.Context, req queue.Request) error {
	return queue.PublishRequest(ctx, a.js, req)
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

// selfNameOperator stores the login user's name as identity.operator when
// the setting is unset and the name is usable: not empty, not "root" (the
// user inside Docker, where os/user is useless anyway), and valid as a
// parent agent id. The caller logs and ignores any error — this is a
// convenience for a bare-metal serve, never a startup failure (D7).
func selfNameOperator(ctx context.Context, res *settings.Resolver) error {
	existing, err := res.String(ctx, settings.KeyIdentityOperator)
	if err != nil {
		return err
	}
	if existing != "" {
		return nil
	}
	cur, err := user.Current()
	if err != nil {
		return err
	}
	if cur.Username == "" || cur.Username == "root" {
		return nil
	}
	if err := agentmeta.ValidateParentAgentID(cur.Username); err != nil {
		return err
	}
	return res.Set(ctx, settings.KeyIdentityOperator, cur.Username)
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

// logStartupKimiBalance refreshes the Kimi account balance once at startup,
// the Kimi counterpart to logStartupBalance: the same warn-only contract,
// against Kimi's own balance endpoint and response shape
// (third_party/kimi-docs/api/balance.md).
func logStartupKimiBalance(ctx context.Context, client *kimi.Client) {
	balCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bal, err := client.GetBalance(balCtx)
	if err != nil {
		log.Printf("harness serve: could not check Kimi account balance at startup: %v", err)
		return
	}
	if bal.Data.AvailableBalance <= 0 {
		log.Printf("harness serve: warning: Kimi available balance is $%.4f (<= 0); kimi-k3 work will fail with exceeded_current_quota_error until it is topped up", bal.Data.AvailableBalance)
		return
	}
	log.Printf("harness serve: Kimi balance available: $%.4f (voucher $%.4f, cash $%.4f)",
		bal.Data.AvailableBalance, bal.Data.VoucherBalance, bal.Data.CashBalance)
}

// logStartupKimiModels fetches the live Kimi model list once at startup and
// warns if the configured model is not on it — the Kimi counterpart to
// logStartupModels, checking only the one model that belongs to Kimi (the
// flash model stays a DeepSeek name and is checked against DeepSeek's list
// by logStartupModels when the default provider is DeepSeek).
func logStartupKimiModels(ctx context.Context, client *kimi.Client, model string) {
	modelsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := client.ListModels(modelsCtx)
	if err != nil {
		log.Printf("harness serve: could not fetch the live Kimi model list at startup: %v", err)
		return
	}
	live := make(map[string]bool, len(resp.Data))
	for _, m := range resp.Data {
		live[m.ID] = true
	}
	if !live[model] {
		log.Printf("harness serve: warning: configured model %q was not in Kimi GET /models' live list", model)
	}
}
