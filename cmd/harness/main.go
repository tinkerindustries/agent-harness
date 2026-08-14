// Command harness is a CLI for talking to DeepSeek's native API directly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/httplog"
	"github.com/mrgeoffrich/deepseek-harness/internal/kimi"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

const usage = `usage: harness <command> [flags]

commands:
  ask "..."                    send a prompt and stream reasoning and content to the terminal
  run -workspace P "..."       run the agent loop against a workspace until it finishes or gives up
  serve                        pull work requests from NATS JetStream and run them as a worker pool,
                               serving the web UI, /api/..., and /mcp on one HTTP port
  publish -repo URL "..."      publish a work request to the queue "harness serve" reads
  resume <session-id> ["..."]  continue a finished, failed, or timed-out session
  stop <session-id> ["reason"] ask the harness to stop a running session
  steer <session-id> "text"    append an instruction to a running session
  delete <session-id>          remove a session and its event log (refuses a running one)
  export <session-id>          rebuild a session's disk mirror from the database
  models                       list available models
  balance                      show account balance
  config                       read and write settings in the database: list, get, set, unset
  worktree <cmd>               allocate per-worktree ports so sibling git worktrees of this
                                repo can run docker-compose.yml and .test.yml concurrently
  eval <cmd>                   measure a prompt change: run a suite under two prompt variants
                                and compare what the sessions did (run, score, variants)

run and publish both require -permission-mode, readonly or full. publish's
-repo takes URL[#branch] and repeats; run's -workspace repeats too, paired
with a -prompt each. "harness <command> -h" lists that command's flags.`

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "harness: warning: reading .env: %v\n", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "ask":
		err = runAsk(ctx, os.Args[2:])
	case "run":
		err = runRun(ctx, os.Args[2:])
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "publish":
		err = runPublish(ctx, os.Args[2:])
	case "resume":
		err = runResume(ctx, os.Args[2:])
	case "stop":
		err = runStop(ctx, os.Args[2:])
	case "steer":
		err = runSteer(ctx, os.Args[2:])
	case "delete":
		err = runDelete(ctx, os.Args[2:])
	case "export":
		err = runExport(ctx, os.Args[2:])
	case "models":
		err = runModels(ctx, os.Args[2:])
	case "balance":
		err = runBalance(ctx, os.Args[2:])
	case "config":
		err = runConfig(ctx, os.Args[2:])
	case "worktree":
		err = runWorktree(ctx, os.Args[2:])
	case "eval":
		err = runEval(ctx, os.Args[2:])
	case "-h", "-help", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness: "+err.Error())
		os.Exit(1)
	}
}

func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("%w (see .env.example)", err)
	}
	return cfg, nil
}

// newHTTPLogRecorder returns a Recorder writing under cfg.HTTPLogRoot, or
// nil when capture is off. Every command that builds one must CloseAll
// before the process exits so each gzip member is closed and the files are
// valid.
func newHTTPLogRecorder(cfg config.Config) *httplog.Recorder {
	if !cfg.HTTPLogEnabled {
		return nil
	}
	return httplog.NewRecorder(cfg.HTTPLogRoot)
}

// closeHTTPLog closes every open session writer rec owns. Nil is a no-op,
// so commands can defer it unconditionally.
func closeHTTPLog(rec *httplog.Recorder) {
	if rec == nil {
		return
	}
	if err := rec.CloseAll(); err != nil {
		log.Printf("harness: close http log: %v", err)
	}
}

// openStore opens (creating if needed) the harness database under
// cfg.DataDir and returns the handle. Every command that reaches the API —
// ask, run, resume, models, balance, serve — holds one, because settings
// (including the DeepSeek API key) live in the database now, not in the
// environment.
func openStore(cfg config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return st, nil
}

// deepSeekAPIKeyProvider returns the key provider the deepseek client calls
// before every request: a read of deepseek.api_key through the store, made
// on every call, so a key set while a process is running takes effect on the
// next request without a restart.
func deepSeekAPIKeyProvider(res *settings.Resolver) func() (string, error) {
	return func() (string, error) {
		return res.DeepSeekAPIKey(context.Background())
	}
}

// kimiAPIKeyProvider returns the key provider the kimi client calls before
// every request: a read of kimi.api_key through the store, made on every
// call, so a key set while a process is running takes effect on the next
// request without a restart.
func kimiAPIKeyProvider(res *settings.Resolver) func() (string, error) {
	return func() (string, error) {
		return res.KimiAPIKey(context.Background())
	}
}

// withHTTPLog wraps a fresh client's transport so every exchange is
// captured by rec. A nil rec (capture off) leaves the request path
// untouched. The client reads its API key from provider before every
// request, so the string passed to NewClient is always the empty placeholder.
func withHTTPLog(cfg config.Config, rec *httplog.Recorder, provider func() (string, error)) *deepseek.Client {
	if rec == nil {
		return deepseek.NewClient(cfg.BaseURL, "", deepseek.WithAPIKeyProvider(provider))
	}
	return deepseek.NewClient(cfg.BaseURL, "", deepseek.WithAPIKeyProvider(provider), deepseek.WithTransportWrapper(func(next http.RoundTripper) http.RoundTripper {
		return httplog.NewTransport(next, rec)
	}))
}

// withKimiHTTPLog mirrors withHTTPLog for the Kimi client: every exchange is
// captured by rec, or the request path is untouched when capture is off. The
// base URL is Moonshot's own (third_party/kimi-docs/api/overview.md); the
// client reads its API key from provider before every request.
func withKimiHTTPLog(cfg config.Config, rec *httplog.Recorder, provider func() (string, error)) *kimi.Client {
	if rec == nil {
		return kimi.NewClient(kimi.DefaultBaseURL, "", kimi.WithAPIKeyProvider(provider))
	}
	return kimi.NewClient(kimi.DefaultBaseURL, "", kimi.WithAPIKeyProvider(provider), kimi.WithTransportWrapper(func(next http.RoundTripper) http.RoundTripper {
		return httplog.NewTransport(next, rec)
	}))
}

// providerFor returns the provider that serves model. A name absent from the
// model→provider table is an operator typo or a retired model; request
// validation rejects it before any of these call sites run, so here the
// DeepSeek fallback keeps the auxiliary commands and the startup logs
// working on the default rather than failing on a stale setting
// (internal/provider, docs/KIMI-INTEGRATION.md §4.3).
func providerFor(model string) provider.Name {
	p, err := provider.ModelFor(model)
	if err != nil {
		return provider.DeepSeek
	}
	return p
}

// clientForModel is the composition point the architecture names: cmd/harness
// builds both provider clients and resolves which one a model speaks to, so
// a shared Runner (or a CLI command) serves whichever provider the request's
// model belongs to without the loop knowing (internal/session/client.go,
// docs/KIMI-INTEGRATION.md §4.3).
func clientForModel(model string, deepSeekClient *deepseek.Client, kimiClient *kimi.Client) session.Client {
	if providerFor(model) == provider.Kimi {
		return kimiClient
	}
	return deepSeekClient
}

// reasoningClaim names, in one short parenthetical fragment, what the
// provider was actually asked to do about reasoning — the wire claim, not
// the session's intent (docs/KIMI-INTEGRATION.md §2). DeepSeek is sent
// `thinking: {type: enabled|disabled}` alongside reasoning_effort, so the
// flag is the truth for it. Kimi K3 takes no thinking field — sending one
// is an API error — and always reasons with Preserved Thinking, its effort
// set by reasoning_effort, so the claim for it is "always reasons" and must
// never read as evidence the harness sent `thinking`
// (third_party/kimi-docs/guide/use-thinking-models.md).
func reasoningClaim(model string, thinking bool) string {
	if providerFor(model) == provider.Kimi {
		return "always reasons"
	}
	if thinking {
		return "thinking enabled"
	}
	return "thinking disabled"
}

// googleAPIKeyProvider returns the key provider the gemini client calls
// before every request: a read of google.api_key through the store, made on
// every call, so a key set while a process is running takes effect on the
// next request without a restart.
func googleAPIKeyProvider(res *settings.Resolver) func() (string, error) {
	return func() (string, error) {
		return res.GoogleAPIKey(context.Background())
	}
}

// googleVisionModelProvider returns the model provider the ReviewScreenshot
// tool calls before every call: a read of google.vision_model through the
// store, defaulting to gemini-3.7-flash when unset, so a model changed
// while a process is running takes effect on the next call without a
// restart.
func googleVisionModelProvider(res *settings.Resolver) func() (string, error) {
	return func() (string, error) {
		return res.GoogleVisionModel(context.Background())
	}
}

// withGeminiHTTPLog wraps a fresh gemini client's transport so every
// exchange is captured by rec, mirroring withHTTPLog. A nil rec (capture
// off) leaves the request path untouched. The client reads its API key from
// provider before every request.
func withGeminiHTTPLog(cfg config.Config, rec *httplog.Recorder, provider func() (string, error)) *gemini.Client {
	if rec == nil {
		return gemini.NewClient(gemini.DefaultBaseURL, gemini.WithAPIKeyProvider(provider))
	}
	return gemini.NewClient(gemini.DefaultBaseURL, gemini.WithAPIKeyProvider(provider), gemini.WithTransportWrapper(func(next http.RoundTripper) http.RoundTripper {
		return httplog.NewTransport(next, rec)
	}))
}

// explainError names an empty account when the error says so, whichever
// provider reported it: DeepSeek answers 402, Kimi answers 429 with error
// type exceeded_current_quota_error (third_party/kimi-docs/api/errors.md,
// api/balance.md) — both mean the operator's fix is a top-up, not a retry.
func explainError(err error) error {
	if deepseek.IsInsufficientBalance(err) || kimi.IsInsufficientBalance(err) {
		return fmt.Errorf("account balance is exhausted: %w", err)
	}
	return err
}

func runAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	model := fs.String("model", "", "override model (default from config)")
	effort := fs.String("effort", "", "override reasoning effort: low, high, max")
	thinking := fs.Bool("thinking", true, "enable thinking mode")
	maxTokens := fs.Int("max-tokens", 0, "override max_tokens (default from config)")
	system := fs.String("system", "", "optional system message")
	jobType := fs.String("job-type", agentmeta.JobTypeImplementation, "implementation or orchestration (default implementation)")
	parentAgentType := fs.String("parent-agent-type", "", "the launching agent's kind, as a lowercase slug (claude-code, cursor, ...)")
	parentAgentID := fs.String("parent-agent-id", "", "the launching agent's session id, or the operator's name with -parent-is-user")
	parentIsUser := fs.Bool("parent-is-user", true, "record this conversation as started by a person, which is the default because harness ask is interactive — pass -parent-is-user=false when scripting it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := agentmeta.ValidateJobType(*jobType); err != nil {
		return err
	}
	if err := agentmeta.ValidateParent(*parentIsUser, *parentAgentType, *parentAgentID); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: harness ask [flags] \"prompt\"")
	}
	prompt := strings.Join(fs.Args(), " ")

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	priceTable, err := pricing.Load(cfg.PriceTablePath)
	if err != nil {
		return err
	}

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	res := settings.NewResolver(st)

	// The ask defaults resolve through the settings registry; the flags
	// below override.
	resolvedModel, err := res.String(ctx, settings.KeyDefaultModel)
	if err != nil {
		return err
	}
	if *model != "" {
		resolvedModel = *model
	}
	resolvedEffort, err := res.String(ctx, settings.KeyDefaultEffort)
	if err != nil {
		return err
	}
	if *effort != "" {
		resolvedEffort = *effort
	}
	resolvedMaxTokens, err := res.Int(ctx, settings.KeyRunMaxTokens)
	if err != nil {
		return err
	}
	if *maxTokens != 0 {
		resolvedMaxTokens = *maxTokens
	}

	rec := newHTTPLogRecorder(cfg)
	defer closeHTTPLog(rec)
	deepSeekClient := withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(res))
	kimiClient := withKimiHTTPLog(cfg, rec, kimiAPIKeyProvider(res))
	// ask speaks to whichever provider the resolved model belongs to, the
	// same per-model routing the Runner uses (clientForModel).
	client := clientForModel(resolvedModel, deepSeekClient, kimiClient)

	var messages []wire.Message
	if *system != "" {
		messages = append(messages, wire.SystemMessage(*system))
	}
	messages = append(messages, wire.UserMessage(prompt))

	// ask states intent and lets the client spell the provider's reasoning
	// control, the same seam the agent loop uses (internal/session/client.go,
	// docs/KIMI-INTEGRATION.md §4.1).
	start := time.Now()
	events, err := client.StreamChatCompletion(ctx, wire.ChatIntent{
		Model:     resolvedModel,
		Messages:  messages,
		Effort:    resolvedEffort,
		Thinking:  *thinking,
		MaxTokens: resolvedMaxTokens,
	})
	if err != nil {
		return explainError(err)
	}

	var reasoningOpen, contentOpen bool
	var contentBuf strings.Builder
	var finishReason string
	var usage *wire.Usage
	var streamErr error

	for ev := range events {
		switch ev.Type {
		case wire.EventReasoningDelta:
			if !reasoningOpen {
				fmt.Println("== reasoning ==")
				reasoningOpen = true
			}
			fmt.Print(ev.Reasoning)
		case wire.EventContentDelta:
			if !contentOpen {
				if reasoningOpen {
					fmt.Println()
				}
				fmt.Println("\n== answer ==")
				contentOpen = true
			}
			fmt.Print(ev.Content)
			contentBuf.WriteString(ev.Content)
		case wire.EventToolCallDelta:
			// `ask` sends no tools, so this should never fire.
			fmt.Printf("\n[unexpected tool call delta: index=%d name=%s]\n", ev.ToolCall.Index, ev.ToolCall.Function.Name)
		case wire.EventFinish:
			finishReason = ev.FinishReason
		case wire.EventUsage:
			usage = ev.Usage
		case wire.EventError:
			streamErr = ev.Err
		}
	}
	elapsed := time.Since(start)
	fmt.Println()

	if streamErr != nil {
		return explainError(fmt.Errorf("stream: %w", streamErr))
	}

	if client.IsReasoningStarved(finishReason, contentBuf.String()) {
		fmt.Fprintln(os.Stderr, "\nreasoning exhausted max_tokens before producing an answer; retry with a larger -max-tokens")
	}

	fmt.Println("\n== usage ==")
	if usage == nil {
		fmt.Println("no usage returned")
		return nil
	}

	reasoningTokens := 0
	if usage.CompletionTokensDetails != nil {
		reasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
	answerTokens := usage.CompletionTokens - reasoningTokens

	// How usage splits into cache hit and miss is the provider's decision —
	// DeepSeek reports the two figures, Kimi a single cached_tokens — so the
	// split comes through the seam (internal/session/client.go).
	cacheHit, cacheMiss := client.UsageSplit(usage)
	// `start` is when the request went out, which is the instant the rate is
	// chosen by from 2026-08-16 — not now, which is after the response came
	// back and can be the other side of a peak boundary.
	cost, rateTier, costErr := priceTable.Cost(resolvedModel, start, cacheHit, cacheMiss, usage.CompletionTokens)

	fmt.Printf("model          %s (effort %s, %s)\n", resolvedModel, resolvedEffort, reasoningClaim(resolvedModel, *thinking))
	fmt.Printf("prompt tokens  %d (cache hit %d / cache miss %d, %s)\n", usage.PromptTokens, cacheHit, cacheMiss, cacheHitRate(cacheHit, cacheMiss))
	fmt.Printf("completion     %d (reasoning %d / answer %d)\n", usage.CompletionTokens, reasoningTokens, answerTokens)
	if costErr == nil {
		fmt.Printf("cost           $%.6f USD (price table captured %s%s)\n", cost, priceTable.CapturedAt, rateTierNote(rateTier))
	} else {
		fmt.Printf("cost           unknown: %v\n", costErr)
	}
	if line := peakWindowLine(priceTable, start); line != "" {
		fmt.Printf("peak hours     %s\n", line)
	}
	fmt.Printf("finish reason  %s\n", finishReason)
	fmt.Printf("wall clock     %s\n", elapsed.Round(time.Millisecond))
	return nil
}

func runModels(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	res := settings.NewResolver(st)
	rec := newHTTPLogRecorder(cfg)
	defer closeHTTPLog(rec)
	deepSeekClient := withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(res))
	kimiClient := withKimiHTTPLog(cfg, rec, kimiAPIKeyProvider(res))

	// The command lists whichever provider the configured default model
	// belongs to: a Kimi installation lists Moonshot's models, a DeepSeek
	// one lists DeepSeek's (docs/KIMI-INTEGRATION.md §5).
	model, err := res.String(ctx, settings.KeyDefaultModel)
	if err != nil {
		return err
	}
	if providerFor(model) == provider.Kimi {
		resp, err := kimiClient.ListModels(ctx)
		if err != nil {
			return explainError(err)
		}
		for _, m := range resp.Data {
			fmt.Printf("%s (owned by %s)\n", m.ID, m.OwnedBy)
		}
		return nil
	}
	resp, err := deepSeekClient.ListModels(ctx)
	if err != nil {
		return explainError(err)
	}
	for _, m := range resp.Data {
		fmt.Printf("%s (owned by %s)\n", m.ID, m.OwnedBy)
	}
	return nil
}

func runBalance(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("balance", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	res := settings.NewResolver(st)
	rec := newHTTPLogRecorder(cfg)
	defer closeHTTPLog(rec)
	deepSeekClient := withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(res))
	kimiClient := withKimiHTTPLog(cfg, rec, kimiAPIKeyProvider(res))

	// Same routing as models: the balance shown is the provider the
	// configured default model runs on, and each provider's response shape
	// differs (DeepSeek's is_available/balance_infos, Kimi's code/data with
	// available/voucher/cash — third_party/kimi-docs/api/balance.md).
	model, err := res.String(ctx, settings.KeyDefaultModel)
	if err != nil {
		return err
	}
	if providerFor(model) == provider.Kimi {
		resp, err := kimiClient.GetBalance(ctx)
		if err != nil {
			return explainError(err)
		}
		fmt.Printf("available: $%.4f (voucher $%.4f, cash $%.4f)\n",
			resp.Data.AvailableBalance, resp.Data.VoucherBalance, resp.Data.CashBalance)
		return nil
	}
	resp, err := deepSeekClient.GetBalance(ctx)
	if err != nil {
		return explainError(err)
	}
	fmt.Printf("available: %v\n", resp.IsAvailable)
	for _, b := range resp.BalanceInfos {
		fmt.Printf("%s  total=%s  granted=%s  topped_up=%s\n", b.Currency, b.TotalBalance, b.GrantedBalance, b.ToppedUpBalance)
	}
	return nil
}

// rateTierNote names the tier a cost was computed at, for the ask command's
// cost line. Flat says nothing: before DeepSeek's split, and for every
// provider that never had one, there is no other tier to have been on and
// the note would be noise on every run.
func rateTierNote(tier pricing.Tier) string {
	switch tier {
	case pricing.TierPeak:
		return ", peak rate"
	case pricing.TierOffPeak:
		return ", off-peak rate"
	default:
		return ""
	}
}

// peakWindowLine renders the price table's peak windows in the machine's own
// timezone, or "" when there is no schedule to render.
//
// Local, not UTC, and this is the one place the operator's zone belongs. What
// a token costs is decided on DeepSeek's clock and nothing about that is
// negotiable by where you are sitting — internal/pricing prices in UTC and
// says so. But "01:00-04:00 UTC" is not a fact anybody can act on, and
// "11:00-14:00" is: it tells an operator at UTC+10 that DeepSeek's expensive
// hours are the middle of their working day, which is the whole reason to
// print it. Both are shown, because the UTC pair is what the pricing page
// says and the local pair is what to do about it.
func peakWindowLine(t *pricing.Table, at time.Time) string {
	if t == nil || t.Schedule == nil {
		return ""
	}
	s := t.Schedule
	local := s.LocalWindows(time.Local)
	parts := make([]string, 0, len(local))
	for _, w := range local {
		part := fmt.Sprintf("%s-%s", w.From, w.To)
		if w.Crossed {
			// A range that ends on the next day reads as an ordinary evening
			// unless it says otherwise.
			part += " (+1d)"
		}
		parts = append(parts, part)
	}
	utc := make([]string, 0, len(s.PeakWindowsUTC))
	for _, w := range s.PeakWindowsUTC {
		utc = append(utc, fmt.Sprintf("%s-%s", w.From, w.To))
	}
	zone, _ := at.In(time.Local).Zone()
	line := fmt.Sprintf("%s %s (%s UTC)", strings.Join(parts, ", "), zone, strings.Join(utc, ", "))
	if at.Before(s.EffectiveAt) {
		line += fmt.Sprintf(" — from %s", s.EffectiveAt.In(time.Local).Format("2006-01-02 15:04 MST"))
	}
	return line
}
