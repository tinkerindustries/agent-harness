package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// runResume continues a session the CLI already ran to a terminal state:
// finished, failed, timed out, or stopped at its sub-turn limit. The
// browser only lists sessions (docs/DESIGN.md §5); resume and delete are
// CLI-only session management (PLAN.md phase 6).
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
	if *maxTokens != 0 {
		cfg.MaxTokens = *maxTokens
	}
	if *maxSubTurns != 0 {
		cfg.MaxSubTurns = *maxSubTurns
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

	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	var resolver tools.Resolver
	if *interactive {
		resolver = newInteractiveResolver()
	}

	r := &session.Runner{
		Store:      st,
		Mirror:     store.NewMirror(cfg.DataDir),
		Client:     deepseek.NewClient(cfg.BaseURL, cfg.APIKey),
		Prices:     priceTable,
		FlashModel: cfg.FlashModel,
	}

	fmt.Printf("resuming %s: %s (effort %s), workspace %s\n\n", sessionID, sess.Model, sess.Effort, sess.Workspace)
	if prompt != "" {
		fmt.Printf("continuation: %s\n\n", prompt)
	}

	res, err := r.Resume(ctx, session.ResumeOptions{
		SessionID: sessionID, Prompt: prompt,
		MaxTokens: cfg.MaxTokens, MaxSubTurns: cfg.MaxSubTurns,
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

	if err := st.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	if err := os.RemoveAll(mirrorDir); err != nil {
		fmt.Fprintf(os.Stderr, "harness: warning: %s was deleted from the database, but removing its mirror directory failed: %v\n", sessionID, err)
	}
	fmt.Printf("deleted %s\n", sessionID)
	return nil
}
