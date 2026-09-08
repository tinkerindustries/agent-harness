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

	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// writeEnvFile writes a KEY=VALUE file and returns its path.
func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// clearKeyEnv unsets both key variables for the duration of one test.
// t.Setenv restores whatever was there, including unset.
func clearKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	os.Unsetenv("GEMINI_API_KEY")
	os.Unsetenv("GOOGLE_API_KEY")
}

func TestGeminiAPIKeyFromTheEnvironment(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("GEMINI_API_KEY", "AI-env")

	key, err := geminiAPIKey("")
	if err != nil {
		t.Fatalf("geminiAPIKey: %v", err)
	}
	if key != "AI-env" {
		t.Errorf("key = %q, want AI-env", key)
	}
}

// GOOGLE_API_KEY is the name Google's own SDKs read, and is used when
// GEMINI_API_KEY is unset.
func TestGeminiAPIKeyFallsBackToGoogleAPIKey(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("GOOGLE_API_KEY", "AI-google")

	key, err := geminiAPIKey("")
	if err != nil {
		t.Fatalf("geminiAPIKey: %v", err)
	}
	if key != "AI-google" {
		t.Errorf("key = %q, want AI-google", key)
	}
}

func TestGeminiAPIKeyFromTheEnvFile(t *testing.T) {
	clearKeyEnv(t)
	path := writeEnvFile(t, "# a comment\nGEMINI_API_KEY=AI-file\nDATABASE_URL=postgres://nope\n")

	key, err := geminiAPIKey(path)
	if err != nil {
		t.Fatalf("geminiAPIKey: %v", err)
	}
	if key != "AI-file" {
		t.Errorf("key = %q, want AI-file", key)
	}
	// Nothing else in the file becomes ambient. Every command a session's
	// Bash tool runs inherits this process's environment, so a repository's
	// own .env must not reach them through the flag.
	if v, set := os.LookupEnv("DATABASE_URL"); set {
		t.Errorf("-env made DATABASE_URL ambient (%q); only the key may be read", v)
	}
}

// The real environment wins, the way it does for every other .env this
// repository reads.
func TestGeminiAPIKeyPrefersTheEnvironmentOverTheFile(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("GEMINI_API_KEY", "AI-env")
	path := writeEnvFile(t, "GEMINI_API_KEY=AI-file\n")

	key, err := geminiAPIKey(path)
	if err != nil {
		t.Fatalf("geminiAPIKey: %v", err)
	}
	if key != "AI-env" {
		t.Errorf("key = %q, want the environment's AI-env", key)
	}
}

// A file that names neither variable is not an error: it leaves the key
// empty, and initialize still succeeds with no key. The first
// interactions.create is what reports it (docs/STDIO-PROTOCOL.md).
func TestGeminiAPIKeyEnvFileWithoutAKeyIsEmpty(t *testing.T) {
	clearKeyEnv(t)
	path := writeEnvFile(t, "SOMETHING=else\n")

	key, err := geminiAPIKey(path)
	if err != nil {
		t.Fatalf("geminiAPIKey: %v", err)
	}
	if key != "" {
		t.Errorf("key = %q, want empty", key)
	}
}

// TestHostedSessionNeverWritesTheKeyToSettings pins design §4.1/§6.4: the
// key this process is handed reaches the Gemini API client without ever
// becoming a row in the state directory's own settings table. stdin is
// already at EOF, so the session starts, sees no interaction, and exits
// cleanly on its own — the same shape as a parent that opened and
// immediately closed the pipe (docs/STDIO-PROTOCOL.md, "the parent exits").
func TestHostedSessionNeverWritesTheKeyToSettings(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("GEMINI_API_KEY", "AI-should-not-be-stored")
	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runGeminiSession(ctx, []string{"-state-dir", dir}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("runGeminiSession: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("open the state directory's database: %v", err)
	}
	defer st.Close()

	res := settings.NewResolver(st)
	key, err := res.GoogleAPIKey(context.Background())
	if err != nil {
		t.Fatalf("GoogleAPIKey: %v", err)
	}
	if key != "" {
		t.Errorf("the settings table holds a Google key: %q", key)
	}

	// Every byte of the database, not just the column this reads: a key
	// that leaked into any row would still be on disk.
	db, err := os.ReadFile(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("read the database: %v", err)
	}
	if bytes.Contains(db, []byte("AI-should-not-be-stored")) {
		t.Error("the API key is on disk in the state directory")
	}
}

// A named file that cannot be read is fatal even when the environment
// already carries a key: a mistyped path is worth hearing about at once,
// not on the machine where the environment happens to be empty.
func TestGeminiAPIKeyMissingEnvFileFails(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("GEMINI_API_KEY", "AI-env")
	missing := filepath.Join(t.TempDir(), "nope.env")

	_, err := geminiAPIKey(missing)
	if err == nil {
		t.Fatal("expected an error for a -env file that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the file, got: %v", err)
	}
}
