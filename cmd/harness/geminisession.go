package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/config"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/geministdio"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// defaultGeminiSessionModel is what a create body with no `model` runs on.
// It is the only Gemini model this harness knows how to route
// (internal/provider), so naming it here is a pointer rather than a second
// source of truth: an unknown model is refused by provider.Known.
const defaultGeminiSessionModel = "gemini-3.7-flash"

// runGeminiSession hosts one coding session for a parent process over in and
// out, speaking the protocol docs/STDIO-PROTOCOL.md describes. main.go calls
// it with os.Stdin and os.Stdout; a test calls it with a pipe, so the
// process's whole setup — credential handling among it — runs without a
// child process to launch or a real stdin to close.
//
// Nothing may reach out but protocol frames. Go's log package writes to
// stderr already; this pins it, because a stray line on stdout would be an
// unparseable frame to the parent and there is no recovering from that.
func runGeminiSession(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("gemini-session", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "directory for this session's own SQLite state and transcript mirror (default: a per-process directory under the user cache dir)")
	keepState := fs.Bool("keep-state", false, "leave the state directory behind when the process exits, for reading a finished session's transcript")
	prices := fs.String("prices", "configs/prices.json", "price table, for the cost figures reported on harness.usage")
	model := fs.String("model", defaultGeminiSessionModel, "model a create body with no `model` runs on")
	envFile := fs.String("env", "", "read the API key from this KEY=VALUE `file` when the environment does not carry one, for driving the process by hand")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log.SetOutput(os.Stderr)
	log.SetPrefix("harness gemini-session: ")

	// The key arrives in the environment the parent spawned this process
	// with. There is no settings store to read one from and no screen to
	// type one into: a hosted session's credentials are the host's to
	// supply. GOOGLE_API_KEY is accepted as well because that is the name
	// Google's own SDKs read. -env names a file to fall back to, for a
	// person driving the process by hand rather than a parent application.
	apiKey, err := geminiAPIKey(*envFile)
	if err != nil {
		return err
	}

	dir := *stateDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "agent-harness", "gemini-session", fmt.Sprint(os.Getpid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	if !*keepState && *stateDir == "" {
		defer os.RemoveAll(dir)
	}

	// The agent loop's state machine is its event log, and the log lives in
	// SQLite (internal/session, internal/store). This is that log and
	// nothing else: no work queue is served, no worker pool claims from it,
	// no HTTP listener reads it. docs/STDIO-PROTOCOL.md records why the
	// binary has a database at all.
	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	// The key reaches the Gemini API client directly, as a closure over this
	// local variable, and never becomes a settings-table row: a -state-dir
	// this process's parent keeps for resuming must not be a file holding
	// the plaintext key (docs/STDIO-PROTOCOL.md, "Trust boundaries";
	// cmd/harness/serve.go's own use of settings.KeyGoogleAPIKey is a
	// different mode — an operator's key, typed into that harness's own
	// settings screen, with a different lifetime — and is unaffected).
	res := settings.NewResolver(st)

	priceTable, err := pricing.Load(*prices)
	if err != nil {
		// A missing price table costs the cost figure on harness.usage and
		// nothing else, so it is a warning rather than a refusal to start.
		log.Printf("no price table (%v); harness.usage will report tokens without a cost", err)
		priceTable = nil
	}

	geminiClient := gemini.NewClient(gemini.DefaultBaseURL, gemini.WithAPIKeyProvider(func() (string, error) {
		return apiKey, nil
	}))

	mcpMgr := mcpclient.New(st)
	mcpMgr.EnvFilter = stripGoogleAPIKeys
	defer mcpMgr.Close()

	eventHub := hub.New()
	runner := &session.Runner{
		Store:         st,
		Mirror:        store.NewMirror(dir),
		Client:        geminiClient,
		ClientFor:     func(string) session.Client { return geminiClient },
		Prices:        priceTable,
		Gemini:        geminiClient,
		FlashModel:    *model,
		Hub:           eventHub,
		Settings:      res,
		GeminiModel:   func() (string, error) { return *model, nil },
		ToolEnvFilter: stripGoogleAPIKeys,
	}

	srv := geministdio.NewServer(geministdio.Options{
		Store:        st,
		Runner:       runner,
		Hub:          eventHub,
		MCP:          mcpMgr,
		Models:       geminiModels(),
		DefaultModel: *model,
		HasAPIKey:    func() bool { return apiKey != "" },
		Version:      buildVersion(),
	})

	log.Printf("ready; state under %s", dir)
	return srv.Serve(ctx, in, out)
}

// geminiModels is the model list the handshake advertises: every model
// internal/provider routes to Google.
func geminiModels() []string {
	var out []string
	for _, m := range provider.KnownModels() {
		if p, err := provider.ModelFor(m); err == nil && p == provider.Gemini {
			out = append(out, m)
		}
	}
	return out
}

// buildVersion is what the handshake reports as this process's version.
// Nothing in this repo stamps one at link time, so it comes from the module
// build info, which is "(devel)" for a local build and the tag for one
// installed from a release.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "unknown"
	}
	return info.Main.Version
}

// geminiAPIKey resolves the Google API key: the environment first, then the
// file -env names, if it named one.
//
// Only the two key variables are taken from that file. The rest of it is
// ignored deliberately, and the file is never made ambient: this process's
// environment is inherited by every command a session's Bash tool runs, and
// the file to hand is usually the .env of the repository the session is
// about to work in. Loading all of it would put that repository's variables
// into the agent's own subprocesses, which is the thing
// docs/STDIO-PROTOCOL.md rules out for the working directory's .env. The
// flag exists so a person can drive the process by hand without exporting a
// key first; it is not a second configuration surface.
//
// The environment wins, the way it does for every other .env this repository
// reads (config.LoadDotEnv). A named file that cannot be read is fatal even
// when the environment already carries a key, because a mistyped path is
// worth hearing about at once rather than on the machine where the
// environment happens to be empty.
func geminiAPIKey(envFile string) (string, error) {
	env := firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY"))
	if envFile == "" {
		return env, nil
	}
	values, err := config.DotEnvValues(envFile)
	if err != nil {
		return "", fmt.Errorf("-env %s: %w", envFile, err)
	}
	return firstNonEmpty(env, values["GEMINI_API_KEY"], values["GOOGLE_API_KEY"]), nil
}

// stripGoogleAPIKeys removes GEMINI_API_KEY and GOOGLE_API_KEY from base,
// which every Bash call and every stdio MCP dial this hosted process makes
// takes as its environment's starting point (tools.Executor.EnvFilter,
// mcpclient.Manager.EnvFilter). Both names are read for the key
// (geminiAPIKey), so both are stripped: a session tool seeing the raw
// variable this process itself was handed would defeat the reason the key
// never reaches the settings table either.
func stripGoogleAPIKeys(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if strings.HasPrefix(kv, "GEMINI_API_KEY=") || strings.HasPrefix(kv, "GOOGLE_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
