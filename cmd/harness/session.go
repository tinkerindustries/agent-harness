package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/mcpclient"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// runResume continues a session the CLI already ran to a terminal state:
// finished, failed, timed out, or stopped at its sub-turn limit.
//
// It runs the loop in this process against this process's own data
// directory, which is what distinguishes it from the browser's resume
// (POST /api/sessions/{id}/resume, docs/RUN-CONTROL.md "Continuing"): that
// one publishes a work request onto a running serve's queue and a worker
// continues the session there. This is the way to continue a session on a
// machine with no server up, and the two do not replace each other. Delete
// remains CLI-only session management.
func runResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	maxTokens := fs.Int("max-tokens", 0, "override max_tokens (default from config)")
	maxSubTurns := fs.Int("max-sub-turns", 0, "override max sub-turns (default from config)")
	interactive := fs.Bool("interactive", false, "prompt on the terminal for calls the permission policy would otherwise deny")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New(`usage: harness resume <session-id> ["additional instructions"]`)
	}
	sessionID := fs.Arg(0)
	prompt := strings.Join(fs.Args()[1:], " ")

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	priceTable, err := pricing.Load(cfg.PriceTablePath)
	if err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	settingsRes := settings.NewResolver(st)

	resolvedMaxTokens, err := settingsRes.Int(ctx, settings.KeyRunMaxTokens)
	if err != nil {
		return err
	}
	if *maxTokens != 0 {
		resolvedMaxTokens = *maxTokens
	}

	// The session's model is resolved before the sub-turn budget: a resumed
	// kimi-k3 session gets K3's own ceiling, every other model the global
	// one (resolveMaxSubTurns, docs/KIMI-INTEGRATION.md §3).
	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	resolvedMaxSubTurns, err := resolveMaxSubTurns(ctx, settingsRes, sess.Model)
	if err != nil {
		return err
	}
	if *maxSubTurns != 0 {
		resolvedMaxSubTurns = *maxSubTurns
	}

	rec := newHTTPLogRecorder(cfg)
	defer closeHTTPLog(rec)

	var resolver tools.Resolver
	if *interactive {
		resolver = newInteractiveResolver()
	}

	// A resumed session speaks to the provider its model belongs to, the
	// same per-model resolver serve and run use (clientForModel). geminiClient
	// is shared between that routing and the Runner's own Gemini field below —
	// one client for both the vision tools and a Gemini coding session.
	deepSeekClient := withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(settingsRes))
	kimiClient := withKimiHTTPLog(cfg, rec, kimiAPIKeyProvider(settingsRes))
	geminiClient := withGeminiHTTPLog(cfg, rec, googleAPIKeyProvider(settingsRes))

	// harness resume reads the same session-level MCP tool_schema the run
	// stored (internal/session/resume.go), but Resume still calls
	// Definitions for the fresh read-only map (docs/MCP.md, "Permissions"),
	// so this command needs its own Manager too.
	mcpMgr := mcpclient.New(st)
	defer mcpMgr.Close()
	// Sampling for a server that has been allowed it (docs/MCP.md,
	// "Sampling"), on the default flash model — this command resolves no
	// flash model of its own, and a server's turn is side work either way.
	mcpMgr.Sampler = deepSeekClient

	r := &session.Runner{
		Store:  st,
		Mirror: store.NewMirror(cfg.DataDir),
		Client: deepSeekClient,
		ClientFor: func(model string) session.Client {
			return clientForModel(model, deepSeekClient, kimiClient, geminiClient)
		},
		Recorder:    rec,
		Prices:      priceTable,
		Gemini:      geminiClient,
		GeminiModel: googleVisionModelProvider(settingsRes),
		Settings:    settingsRes,
		MCP:         mcpMgr,
	}

	fmt.Printf("resuming %s: %s (effort %s), workspace %s\n\n", sessionID, sess.Model, sess.Effort, sess.Workspace)
	if prompt != "" {
		fmt.Printf("continuation: %s\n\n", prompt)
	}

	res, err := r.Resume(ctx, session.ResumeOptions{
		SessionID: sessionID, Prompt: prompt,
		MaxTokens: resolvedMaxTokens, MaxSubTurns: resolvedMaxSubTurns,
		Resolver: resolver,
		Progress: printProgress,
	})
	if err != nil {
		return explainError(err)
	}
	printResult(res, priceTable)
	fmt.Printf("mirror: %s\n", filepath.Join(cfg.DataDir, "sessions"))
	return nil
}

// runDelete removes a session and its whole event log from the database,
// and its disk mirror directory alongside it. It refuses (through
// Store.DeleteSession) a session that is still running.
func runDelete(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: harness delete <session-id>")
	}
	sessionID := fs.Arg(0)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	mirrorDir := store.NewMirror(cfg.DataDir).Dir(sess)

	// The version the read above returned is the optimistic-concurrency
	// precondition (docs/DATA-API.md): a row that changed between the read
	// and the delete refuses rather than being deleted by a stale decision.
	if err := st.DeleteSession(ctx, sessionID, sess.Version); err != nil {
		return err
	}
	if err := os.RemoveAll(mirrorDir); err != nil {
		fmt.Fprintf(os.Stderr, "harness: warning: %s was deleted from the database, but removing its mirror directory failed: %v\n", sessionID, err)
	}
	fmt.Printf("deleted %s\n", sessionID)
	return nil
}
