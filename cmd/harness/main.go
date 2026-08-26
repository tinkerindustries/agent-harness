// Command harness is the server: it serves the web UI, the /api/... HTTP
// API, and the MCP launch server at /mcp on one HTTP port, and runs the
// worker pool that claims work off the durable queue and drives each agent
// session. The same binary also carries worktree, repository development
// infrastructure unrelated to any of that — it allocates the per-worktree
// ports and compose project names that let sibling git worktrees of this
// repo run concurrently.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mrgeoffrich/agent-harness/internal/config"
	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/httplog"
	"github.com/mrgeoffrich/agent-harness/internal/kimi"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

const usage = `usage: harness <command> [flags]

commands:
  serve            pull work requests off the durable work queue and run them as a worker pool,
                    serving the web UI, /api/..., and /mcp on one HTTP port
  worktree <cmd>    allocate per-worktree ports so sibling git worktrees of this
                    repo can run docker-compose.yml and .test.yml concurrently

"harness <command> -h" lists that command's flags.`

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
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "worktree":
		err = runWorktree(ctx, os.Args[2:])
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
// cfg.DataDir and returns the handle. serve holds one for as long as it
// runs, because settings (including the DeepSeek API key) live in the
// database now, not in the environment.
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
// DeepSeek fallback keeps the startup logs working on the default rather
// than failing on a stale setting (internal/provider,
// docs/KIMI-INTEGRATION.md §4.3).
func providerFor(model string) provider.Name {
	p, err := provider.ModelFor(model)
	if err != nil {
		return provider.DeepSeek
	}
	return p
}

// clientForModel is the composition point the architecture names: cmd/harness
// builds all three provider clients and resolves which one a model speaks
// to, so a shared Runner serves whichever provider the request's model
// belongs to without the loop knowing
// (internal/session/client.go, docs/KIMI-INTEGRATION.md §4.3,
// docs/GEMINI-INTEGRATION.md §7 Phase 5). geminiClient is the same
// *gemini.Client a Runner's Gemini field holds for the vision tools — one
// client, one transport, two callers (Interact for vision, StreamChatCompletion
// / CreateChatCompletion for this seam) — so routing a coding session onto
// Gemini opens no second connection and needs no second API key.
func clientForModel(model string, deepSeekClient *deepseek.Client, kimiClient *kimi.Client, geminiClient *gemini.Client) session.Client {
	switch providerFor(model) {
	case provider.Kimi:
		return kimiClient
	case provider.Gemini:
		return geminiClient
	default:
		return deepSeekClient
	}
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
