package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/provider"
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

// clearKeyEnv unsets every provider key variable for the duration of one
// test. t.Setenv restores whatever was there, including unset.
//
// DEEPSEEK_API_KEY is cleared along with Google's two because this
// repository's own developers have it set: a test that asserted on the
// absence of a DeepSeek key would otherwise pass on CI and fail on the
// machine the harness is written on.
func clearKeyEnv(t *testing.T) {
	t.Helper()
	for _, name := range providerAPIKeyVars {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func TestGeminiAPIKeyFromTheEnvironment(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("GEMINI_API_KEY", "AI-env")

	key, _, err := apiKeys("")
	if err != nil {
		t.Fatalf("apiKeys: %v", err)
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

	key, _, err := apiKeys("")
	if err != nil {
		t.Fatalf("apiKeys: %v", err)
	}
	if key != "AI-google" {
		t.Errorf("key = %q, want AI-google", key)
	}
}

func TestGeminiAPIKeyFromTheEnvFile(t *testing.T) {
	clearKeyEnv(t)
	path := writeEnvFile(t, "# a comment\nGEMINI_API_KEY=AI-file\nDATABASE_URL=postgres://nope\n")

	key, _, err := apiKeys(path)
	if err != nil {
		t.Fatalf("apiKeys: %v", err)
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

	key, _, err := apiKeys(path)
	if err != nil {
		t.Fatalf("apiKeys: %v", err)
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

	key, _, err := apiKeys(path)
	if err != nil {
		t.Fatalf("apiKeys: %v", err)
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
	if err := runStdioSession(ctx, "stdio-session", []string{"-state-dir", dir}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("runStdioSession: %v", err)
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

	_, _, err := apiKeys(missing)
	if err == nil {
		t.Fatal("expected an error for a -env file that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the file, got: %v", err)
	}
}

// TestHostedModelsIsTheAdvertisedSet pins what this command offers: every
// Gemini model the repository routes, plus the one DeepSeek model
// internal/provider routes, deepseek-flash — the only one that reads images
// natively (docs/DEEPSEEK-VISION.md).
func TestHostedModelsIsTheAdvertisedSet(t *testing.T) {
	hosted := hostedModels(false)

	if !slices.Contains(hosted, deepSeekSessionModel) {
		t.Errorf("hostedModels() = %v, want the vision model in it", hosted)
	}
	if !slices.Contains(hosted, defaultGeminiSessionModel) {
		t.Errorf("hostedModels() = %v, want the default Gemini model in it", hosted)
	}
	for _, unwanted := range []string{"kimi-k3"} {
		if slices.Contains(hosted, unwanted) {
			t.Errorf("hostedModels() offers %s; this command hosts only %s from that provider",
				unwanted, deepSeekSessionModel)
		}
	}
	// Every Gemini model the repository routes is offered, so a model added
	// to internal/provider reaches a hosted session without an edit here.
	for _, m := range provider.KnownModels() {
		if p, err := provider.ModelFor(m); err == nil && p == provider.Gemini && !slices.Contains(hosted, m) {
			t.Errorf("hostedModels() = %v, missing the routed Gemini model %s", hosted, m)
		}
	}
	// Every hosted model reads images natively. That is the property that
	// makes one credential enough: a model that could not see would be
	// handed the vision tools, and those reach Google.
	for _, m := range hosted {
		if !provider.SeesImages(m) {
			t.Errorf("hostedModels() offers %s, which does not read images natively", m)
		}
	}
}

// The default model follows the credentials the host supplied: a host that
// gave one key meant the model that key runs.
func TestResolveHostedModel(t *testing.T) {
	cases := []struct {
		name                 string
		named, google, dsKey string
		want                 string
		wantErr              bool
	}{
		{name: "both keys, nothing named", google: "g", dsKey: "d", want: defaultGeminiSessionModel},
		{name: "no keys at all", want: defaultGeminiSessionModel},
		{name: "only a DeepSeek key", dsKey: "d", want: deepSeekSessionModel},
		{name: "only a Google key", google: "g", want: defaultGeminiSessionModel},
		{name: "a named model wins over the keys", named: deepSeekSessionModel, google: "g", want: deepSeekSessionModel},
		{name: "a named Gemini model", named: "gemini-3.5-flash", dsKey: "d", want: "gemini-3.5-flash"},
		{name: "a model this command does not host", named: "kimi-k3", google: "g", wantErr: true},
		{name: "a model nothing routes", named: "gpt-9", google: "g", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveHostedModel(tc.named, hostedModels(false), tc.google, tc.dsKey)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveHostedModel(%q) = %q, want an error naming the hosted set", tc.named, got)
				}
				// The parent hears which names it may use, at startup,
				// rather than on its first interaction.
				if !strings.Contains(err.Error(), deepSeekSessionModel) {
					t.Errorf("the error does not name the hosted set: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveHostedModel: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveHostedModel = %q, want %q", got, tc.want)
			}
		})
	}
}

// The vision tools reach Google whatever the session runs on, so the model
// they resolve stays a Gemini one even for a DeepSeek session. No hosted
// model is offered those tools today — every one of them sees images itself
// — so this is what the fallback would have to be, not something in use.
func TestGeminiVisionModelStaysGooglesEvenForADeepSeekSession(t *testing.T) {
	if got := geminiVisionModel(deepSeekSessionModel); got != defaultGeminiSessionModel {
		t.Errorf("geminiVisionModel(%s) = %q, want %q", deepSeekSessionModel, got, defaultGeminiSessionModel)
	}
	if got := geminiVisionModel("gemini-3.5-flash"); got != "gemini-3.5-flash" {
		t.Errorf("geminiVisionModel = %q, want the session's own Gemini model", got)
	}
}

// The DeepSeek key is read from the same two places Google's is, and neither
// provider's key is required for the other's to work.
func TestAPIKeysReadsBothProviders(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-env")

	google, deepSeek, err := apiKeys("")
	if err != nil {
		t.Fatalf("apiKeys: %v", err)
	}
	if deepSeek != "sk-env" {
		t.Errorf("DeepSeek key = %q, want sk-env", deepSeek)
	}
	if google != "" {
		t.Errorf("Google key = %q, want empty", google)
	}

	clearKeyEnv(t)
	path := writeEnvFile(t, "DEEPSEEK_API_KEY=sk-file\nGEMINI_API_KEY=AI-file\nDATABASE_URL=postgres://nope\n")
	google, deepSeek, err = apiKeys(path)
	if err != nil {
		t.Fatalf("apiKeys: %v", err)
	}
	if deepSeek != "sk-file" || google != "AI-file" {
		t.Errorf("keys = (%q, %q), want (AI-file, sk-file)", google, deepSeek)
	}
	if v, set := os.LookupEnv("DATABASE_URL"); set {
		t.Errorf("-env made DATABASE_URL ambient (%q); only the keys may be read", v)
	}
}

// Every variable a provider key is read from is stripped from what a
// session's tools inherit — including DeepSeek's, which matters most in this
// repository: a session working here has a harness of its own that reads it.
func TestStripProviderAPIKeysRemovesEveryProvidersKey(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"GEMINI_API_KEY=AI-1",
		"GOOGLE_API_KEY=AI-2",
		"DEEPSEEK_API_KEY=sk-1",
		"DEEPSEEK_BASE_URL=https://api.deepseek.com",
		"HOME=/home/agent",
	}
	got := stripProviderAPIKeys(base)

	for _, kv := range got {
		for _, name := range providerAPIKeyVars {
			if strings.HasPrefix(kv, name+"=") {
				t.Errorf("%s survived the filter", name)
			}
		}
	}
	// Everything else survives, including a variable whose name merely
	// starts the same way: the filter matches an assignment, not a prefix.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/agent", "DEEPSEEK_BASE_URL=https://api.deepseek.com"} {
		if !slices.Contains(got, want) {
			t.Errorf("the filter dropped %q", want)
		}
	}
}

// TestTheSubcommandPicksTheDialect pins what each name gets: the handshake
// reports the name it was spawned as, the protocol that name speaks, and the
// models that name hosts.
//
// `gemini-session` is not an alias. It speaks Google's Interactions
// vocabulary and hosts Google's models; `stdio-session` speaks the Responses
// vocabulary and hosts the DeepSeek vision model as well. A client picks its
// command by the vocabulary it implements (docs/STDIO-PROTOCOL.md and
// docs/STDIO-INTERACTIONS.md, "Starting the process").
func TestTheSubcommandPicksTheDialect(t *testing.T) {
	clearKeyEnv(t)
	for _, tc := range []struct {
		invoked      string
		protocol     string
		wantDeepSeek bool
	}{
		{"stdio-session", "openai.responses.v1", true},
		{"gemini-session", "google.interactions.v1beta", false},
	} {
		invoked := tc.invoked
		t.Run(invoked, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
				`"params":{"client_info":{"name":"test","version":"0"},"capabilities":{}}}` + "\n")
			var out bytes.Buffer
			if err := runStdioSession(ctx, invoked, []string{"-state-dir", t.TempDir()}, in, &out); err != nil {
				t.Fatalf("runStdioSession: %v", err)
			}

			var frame struct {
				Result struct {
					ServerInfo struct {
						Name     string `json:"name"`
						Protocol string `json:"protocol"`
					} `json:"server_info"`
					Models []string `json:"models"`
				} `json:"result"`
			}
			line, _, _ := strings.Cut(out.String(), "\n")
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("decode the handshake frame %q: %v", line, err)
			}

			if want := "agent-harness " + invoked; frame.Result.ServerInfo.Name != want {
				t.Errorf("server_info.name = %q, want %q", frame.Result.ServerInfo.Name, want)
			}
			if frame.Result.ServerInfo.Protocol != tc.protocol {
				t.Errorf("server_info.protocol = %q, want %q", frame.Result.ServerInfo.Protocol, tc.protocol)
			}
			// Every command hosts Google's models. Only the Responses one
			// hosts DeepSeek's, because a client speaking Google's
			// vocabulary has no way to drive a model of another vendor's
			// through it.
			if !slices.Contains(frame.Result.Models, defaultGeminiSessionModel) {
				t.Errorf("models = %v, want the default Gemini model in it", frame.Result.Models)
			}
			if got := slices.Contains(frame.Result.Models, deepSeekSessionModel); got != tc.wantDeepSeek {
				t.Errorf("models = %v, DeepSeek model present = %v, want %v", frame.Result.Models, got, tc.wantDeepSeek)
			}
		})
	}
}

// TestGeminiSessionNeverDefaultsToDeepSeek pins the one place the two
// commands' credential handling differs.
//
// Started with only a DeepSeek key, `stdio-session` defaults to the DeepSeek
// model: a host that supplied one key meant the model that key runs.
// `gemini-session` cannot, because it does not host that model — a client
// speaking Google's vocabulary would be driving it through
// generation_config.thinking_level and reading its answers as Google steps.
// It keeps the Google default, and the first create fails with -32003 naming
// the variable a Google model needs.
func TestGeminiSessionNeverDefaultsToDeepSeek(t *testing.T) {
	responses := hostedModels(false)
	interactions := hostedModels(true)

	got, err := resolveHostedModel("", responses, "", "a-deepseek-key")
	if err != nil {
		t.Fatalf("stdio-session with only a DeepSeek key: %v", err)
	}
	if got != deepSeekSessionModel {
		t.Errorf("stdio-session default = %q, want %q", got, deepSeekSessionModel)
	}

	got, err = resolveHostedModel("", interactions, "", "a-deepseek-key")
	if err != nil {
		t.Fatalf("gemini-session with only a DeepSeek key: %v", err)
	}
	if got != defaultGeminiSessionModel {
		t.Errorf("gemini-session default = %q, want %q", got, defaultGeminiSessionModel)
	}

	// Naming it outright is refused there too, at startup rather than on
	// the first create.
	if _, err := resolveHostedModel(deepSeekSessionModel, interactions, "", "a-deepseek-key"); err == nil {
		t.Error("gemini-session accepted -model " + deepSeekSessionModel)
	}
}
