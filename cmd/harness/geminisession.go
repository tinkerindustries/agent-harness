package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"

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

// runGeminiSession hosts one coding session for a parent process over stdin
// and stdout, speaking the protocol docs/STDIO-PROTOCOL.md describes.
//
// Nothing may reach stdout but protocol frames. Go's log package writes to
// stderr already; this pins it, because a stray line on stdout would be an
// unparseable frame to the parent and there is no recovering from that.
func runGeminiSession(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gemini-session", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "directory for this session's own SQLite state and transcript mirror (default: a per-process directory under the user cache dir)")
	keepState := fs.Bool("keep-state", false, "leave the state directory behind when the process exits, for reading a finished session's transcript")
	prices := fs.String("prices", "configs/prices.json", "price table, for the cost figures reported on harness.usage")
	model := fs.String("model", defaultGeminiSessionModel, "model a create body with no `model` runs on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log.SetOutput(os.Stderr)
	log.SetPrefix("harness gemini-session: ")

	// The key arrives in the environment the parent spawned this process
	// with. There is no settings store to read one from and no screen to
	// type one into: a hosted session's credentials are the host's to
	// supply. GOOGLE_API_KEY is accepted as well because that is the name
	// Google's own SDKs read.
	apiKey := firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY"))

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

	res := settings.NewResolver(st)
	if apiKey != "" {
		if err := res.Set(ctx, settings.KeyGoogleAPIKey, apiKey); err != nil {
			return fmt.Errorf("record the API key: %w", err)
		}
	}

	priceTable, err := pricing.Load(*prices)
	if err != nil {
		// A missing price table costs the cost figure on harness.usage and
		// nothing else, so it is a warning rather than a refusal to start.
		log.Printf("no price table (%v); harness.usage will report tokens without a cost", err)
		priceTable = nil
	}

	geminiClient := gemini.NewClient(gemini.DefaultBaseURL, gemini.WithAPIKeyProvider(func() (string, error) {
		return res.GoogleAPIKey(ctx)
	}))

	mcpMgr := mcpclient.New(st)
	defer mcpMgr.Close()

	eventHub := hub.New()
	runner := &session.Runner{
		Store:       st,
		Mirror:      store.NewMirror(dir),
		Client:      geminiClient,
		ClientFor:   func(string) session.Client { return geminiClient },
		Prices:      priceTable,
		Gemini:      geminiClient,
		FlashModel:  *model,
		Hub:         eventHub,
		Settings:    res,
		GeminiModel: func() (string, error) { return *model, nil },
	}

	srv := geministdio.NewServer(geministdio.Options{
		Store:        st,
		Runner:       runner,
		Hub:          eventHub,
		MCP:          mcpMgr,
		Models:       geminiModels(),
		DefaultModel: *model,
		HasAPIKey: func() bool {
			k, err := res.GoogleAPIKey(ctx)
			return err == nil && k != ""
		},
		Version: buildVersion(),
	})

	log.Printf("ready; state under %s", dir)
	return srv.Serve(ctx, os.Stdin, os.Stdout)
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
