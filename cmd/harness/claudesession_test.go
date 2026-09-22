package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

func TestClaudeAPIKeyFromTheEnvironmentAndFile(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "ak-env")

	key, err := claudeAPIKey("")
	if err != nil {
		t.Fatalf("claudeAPIKey: %v", err)
	}
	if key != "ak-env" {
		t.Errorf("key = %q, want ak-env", key)
	}

	clearKeyEnv(t)
	path := writeEnvFile(t, "ANTHROPIC_API_KEY=ak-file\n")
	key, err = claudeAPIKey(path)
	if err != nil {
		t.Fatalf("claudeAPIKey: %v", err)
	}
	if key != "ak-file" {
		t.Errorf("key = %q, want ak-file", key)
	}
}

// TestClaudeHostedModelsIsAllThreeClaudeModels pins the model list
// claude-session advertises: every Claude model internal/provider routes,
// unconditionally — unlike stdio-session's mixed list, this subcommand
// hosts one provider's models alone.
func TestClaudeHostedModelsIsAllThreeClaudeModels(t *testing.T) {
	got := claudeHostedModels()
	want := []string{"claude-opus-5-5", "claude-sonnet-5", "claude-fable-5-1"}
	for _, m := range want {
		found := false
		for _, g := range got {
			if g == m {
				found = true
			}
		}
		if !found {
			t.Errorf("claudeHostedModels() = %v, missing %s", got, m)
		}
	}
	if len(got) != len(want) {
		t.Errorf("claudeHostedModels() = %v, want exactly %v", got, want)
	}
}

// TestClaudeSessionRefusesAnUnknownModel pins -model's validation against
// the hosted list, at startup rather than at the first create.
func TestClaudeSessionRefusesAnUnknownModel(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "ak-test")
	dir := t.TempDir()

	err := runClaudeSession(context.Background(), []string{"-state-dir", dir, "-model", "gemini-3.7-flash"}, strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("a model claude-session does not host was accepted")
	}
}

// TestClaudeSessionNeverWritesTheKeyToSettings mirrors
// TestHostedSessionNeverWritesTheKeyToSettings for the Anthropic key: stdin
// is already at EOF, so the session starts, sees no interaction, and exits
// cleanly on its own.
func TestClaudeSessionNeverWritesTheKeyToSettings(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "ak-should-not-be-stored")
	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runClaudeSession(ctx, []string{"-state-dir", dir}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("runClaudeSession: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("open the state directory's database: %v", err)
	}
	st.Close()

	db, err := os.ReadFile(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("read the database: %v", err)
	}
	if bytes.Contains(db, []byte("ak-should-not-be-stored")) {
		t.Error("the API key is on disk in the state directory")
	}
}
