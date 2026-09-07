package githubauth

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// fakeStore is the settings surface of *store.Store, backed by a map — the
// same pattern internal/settings/settings_test.go uses to test the resolver
// without a database.
type fakeStore struct {
	values map[string]string
}

func (f *fakeStore) Setting(ctx context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeStore) SetSetting(ctx context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakeStore) DeleteSetting(ctx context.Context, key string) error {
	delete(f.values, key)
	return nil
}

// clearGithubEnv unsets the three ambient variables Sync touches before and
// after each test, so one test's environment never leaks into the next —
// os.Setenv/Unsetenv change process-global state, and t.Parallel is not
// used anywhere in this package for exactly that reason.
func clearGithubEnv(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, key := range []string{
			"GITHUB_TOKEN", "GH_TOKEN",
			"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
			"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1",
			EnvCredentialURL, EnvCredentialToken,
		} {
			os.Unsetenv(key)
		}
	}
	clear()
	t.Cleanup(clear)
}

func TestSyncWritesCredentialFileAndEnv(t *testing.T) {
	clearGithubEnv(t)
	res := settings.NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()
	credPath := filepath.Join(t.TempDir(), "git-credentials")

	if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_abc123"); err != nil {
		t.Fatalf("Set github.token: %v", err)
	}
	if err := Sync(ctx, res, Config{CredentialPath: credPath}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat %s: %v", credPath, err)
	}
	// The file holds a GitHub token, so on the platforms the harness runs on
	// it must not be group- or world-readable. Windows has no unix mode: Go
	// maps the 0600 given to os.WriteFile onto the read-only attribute alone
	// and Perm reports 0666 whatever was asked for, so there is nothing here
	// to assert. Confining the token on Windows would take an ACL, which
	// nothing in this repository does — a green run here is not evidence the
	// file is protected there.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("credential file mode = %o, want 0600", perm)
		}
	}
	got, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("read %s: %v", credPath, err)
	}
	want := "https://x-access-token:ghp_abc123@github.com\n"
	if string(got) != want {
		t.Errorf("credential file content = %q, want %q", got, want)
	}

	if v := os.Getenv("GITHUB_TOKEN"); v != "ghp_abc123" {
		t.Errorf("GITHUB_TOKEN = %q, want ghp_abc123", v)
	}
	if v := os.Getenv("GH_TOKEN"); v != "ghp_abc123" {
		t.Errorf("GH_TOKEN = %q, want ghp_abc123", v)
	}
	if v := os.Getenv("GIT_CONFIG_COUNT"); v != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 1", v)
	}
	if v := os.Getenv("GIT_CONFIG_KEY_0"); v != "credential.helper" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want credential.helper", v)
	}
	want = "store --file=" + credPath
	if v := os.Getenv("GIT_CONFIG_VALUE_0"); v != want {
		t.Errorf("GIT_CONFIG_VALUE_0 = %q, want %q", v, want)
	}
}

// TestSyncEmptyTokenRemovesFileAndUnsetsEnv pins the other half of Sync's
// contract: a token set and then unset leaves nothing behind — the
// credential file a previous Sync wrote is removed, and GITHUB_TOKEN /
// GH_TOKEN no longer read the stale value.
func TestSyncEmptyTokenRemovesFileAndUnsetsEnv(t *testing.T) {
	clearGithubEnv(t)
	res := settings.NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()
	credPath := filepath.Join(t.TempDir(), "git-credentials")

	if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_abc123"); err != nil {
		t.Fatalf("Set github.token: %v", err)
	}
	if err := Sync(ctx, res, Config{CredentialPath: credPath}); err != nil {
		t.Fatalf("Sync (set): %v", err)
	}
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("credential file should exist after the first Sync: %v", err)
	}

	if err := res.Unset(ctx, settings.KeyGitHubToken); err != nil {
		t.Fatalf("Unset github.token: %v", err)
	}
	if err := Sync(ctx, res, Config{CredentialPath: credPath}); err != nil {
		t.Fatalf("Sync (unset): %v", err)
	}

	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("credential file should be removed, stat err = %v", err)
	}
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		t.Errorf("GITHUB_TOKEN = %q, want unset", v)
	}
	if v := os.Getenv("GH_TOKEN"); v != "" {
		t.Errorf("GH_TOKEN = %q, want unset", v)
	}
}

// TestSyncEmptyTokenOnAFreshCredPathIsNotAnError pins that removing a
// credential file that was never written — the very first Sync of a fresh
// install with no token ever set — is not an error.
func TestSyncEmptyTokenOnAFreshCredPathIsNotAnError(t *testing.T) {
	clearGithubEnv(t)
	res := settings.NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()
	credPath := filepath.Join(t.TempDir(), "git-credentials")

	if err := Sync(ctx, res, Config{CredentialPath: credPath}); err != nil {
		t.Fatalf("Sync on a store with no token: %v", err)
	}
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("credential file should not exist, stat err = %v", err)
	}
}

func TestSeedFromEnvOnlyFiresWhenUnset(t *testing.T) {
	clearGithubEnv(t)
	ctx := context.Background()

	t.Run("seeds an unset setting from the environment", func(t *testing.T) {
		os.Setenv("GITHUB_TOKEN", "ghp_fromenv")
		t.Cleanup(func() { os.Unsetenv("GITHUB_TOKEN") })
		res := settings.NewResolver(&fakeStore{values: map[string]string{}})

		seeded, err := SeedFromEnv(ctx, res)
		if err != nil {
			t.Fatalf("SeedFromEnv: %v", err)
		}
		if !seeded {
			t.Fatal("SeedFromEnv reported false, want true")
		}
		got, err := res.String(ctx, settings.KeyGitHubToken)
		if err != nil || got != "ghp_fromenv" {
			t.Fatalf("github.token = %q err=%v, want ghp_fromenv nil", got, err)
		}
	})

	t.Run("does not fire when the environment is empty", func(t *testing.T) {
		res := settings.NewResolver(&fakeStore{values: map[string]string{}})

		seeded, err := SeedFromEnv(ctx, res)
		if err != nil {
			t.Fatalf("SeedFromEnv: %v", err)
		}
		if seeded {
			t.Fatal("SeedFromEnv reported true with no GITHUB_TOKEN in the environment")
		}
	})

	t.Run("does not overwrite an already-set setting", func(t *testing.T) {
		os.Setenv("GITHUB_TOKEN", "ghp_fromenv")
		t.Cleanup(func() { os.Unsetenv("GITHUB_TOKEN") })
		res := settings.NewResolver(&fakeStore{values: map[string]string{}})
		if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_alreadystored"); err != nil {
			t.Fatalf("Set github.token: %v", err)
		}

		seeded, err := SeedFromEnv(ctx, res)
		if err != nil {
			t.Fatalf("SeedFromEnv: %v", err)
		}
		if seeded {
			t.Fatal("SeedFromEnv reported true with the setting already stored")
		}
		got, err := res.String(ctx, settings.KeyGitHubToken)
		if err != nil || got != "ghp_alreadystored" {
			t.Fatalf("github.token = %q err=%v, want ghp_alreadystored nil (unchanged)", got, err)
		}
	})

	t.Run("seeds again once the setting is cleared, if the env var is still set", func(t *testing.T) {
		os.Setenv("GITHUB_TOKEN", "ghp_fromenv")
		t.Cleanup(func() { os.Unsetenv("GITHUB_TOKEN") })
		res := settings.NewResolver(&fakeStore{values: map[string]string{}})
		if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_alreadystored"); err != nil {
			t.Fatalf("Set github.token: %v", err)
		}
		if err := res.Unset(ctx, settings.KeyGitHubToken); err != nil {
			t.Fatalf("Unset github.token: %v", err)
		}

		seeded, err := SeedFromEnv(ctx, res)
		if err != nil {
			t.Fatalf("SeedFromEnv: %v", err)
		}
		if !seeded {
			t.Fatal("SeedFromEnv reported false after the setting was cleared with GITHUB_TOKEN still set")
		}
	})
}

// fakeApp stands in for *githubapp.Provider: Sync only ever asks whether an
// App is configured, which is the whole of the AppProvider seam.
type fakeApp struct {
	configured bool
	err        error
}

func (f fakeApp) Configured(ctx context.Context) (bool, error) { return f.configured, f.err }

// TestSyncAppPathPointsGitAtTheHelper pins what a configured GitHub App
// changes: no credential file (an App has no standing token to write into
// one), git pointed at the helper with useHttpPath so the helper is told
// which repository it is being asked about, the callback address and token
// published for the helper to find, and GITHUB_TOKEN/GH_TOKEN unset —
// no single token is right for every account, so gh is given one per
// session instead (cmd/harness/github.go).
func TestSyncAppPathPointsGitAtTheHelper(t *testing.T) {
	clearGithubEnv(t)
	res := settings.NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()
	credPath := filepath.Join(t.TempDir(), "git-credentials")

	// A token is stored as well, to pin that the App wins over it.
	if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_abc123"); err != nil {
		t.Fatalf("Set github.token: %v", err)
	}
	cfg := Config{
		CredentialPath:  credPath,
		App:             fakeApp{configured: true},
		HelperCommand:   "/usr/local/bin/harness github-credential",
		CredentialURL:   "http://127.0.0.1:8080/api/github/credential",
		CredentialToken: "cred-token",
	}
	if err := Sync(ctx, res, cfg); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Errorf("credential file should not exist on the App path, stat err = %v", err)
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "credential.helper",
		"GIT_CONFIG_VALUE_0": "!/usr/local/bin/harness github-credential",
		"GIT_CONFIG_KEY_1":   "credential.useHttpPath",
		"GIT_CONFIG_VALUE_1": "true",
		EnvCredentialURL:     "http://127.0.0.1:8080/api/github/credential",
		EnvCredentialToken:   "cred-token",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		t.Errorf("GITHUB_TOKEN = %q, want unset on the App path", v)
	}
	if v := os.Getenv("GH_TOKEN"); v != "" {
		t.Errorf("GH_TOKEN = %q, want unset on the App path", v)
	}
}

// TestSyncFallsBackToTheTokenWithoutAHelper pins the other half of the
// decision: an App configured in the settings but no helper command to run
// it takes the token path, because an App with no way to mint its tokens
// would leave git with no credential at all.
func TestSyncFallsBackToTheTokenWithoutAHelper(t *testing.T) {
	clearGithubEnv(t)
	res := settings.NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()
	credPath := filepath.Join(t.TempDir(), "git-credentials")
	if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_abc123"); err != nil {
		t.Fatalf("Set github.token: %v", err)
	}

	if err := Sync(ctx, res, Config{CredentialPath: credPath, App: fakeApp{configured: true}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("credential file should exist on the token path: %v", err)
	}
	if v := os.Getenv("GH_TOKEN"); v != "ghp_abc123" {
		t.Errorf("GH_TOKEN = %q, want ghp_abc123", v)
	}
	if v := os.Getenv("GIT_CONFIG_COUNT"); v != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 1", v)
	}
}

// TestSyncSwitchingBackToTheTokenClearsTheAppEntries pins that the two
// paths leave nothing of each other behind: the App path's second
// GIT_CONFIG entry and its callback variables are gone after a Sync that
// takes the token path, so nothing raising GIT_CONFIG_COUNT later could
// revive a credential.useHttpPath nobody asked for.
func TestSyncSwitchingBackToTheTokenClearsTheAppEntries(t *testing.T) {
	clearGithubEnv(t)
	res := settings.NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()
	credPath := filepath.Join(t.TempDir(), "git-credentials")
	if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_abc123"); err != nil {
		t.Fatalf("Set github.token: %v", err)
	}
	appCfg := Config{
		CredentialPath:  credPath,
		App:             fakeApp{configured: true},
		HelperCommand:   "harness github-credential",
		CredentialURL:   "http://127.0.0.1:8080/api/github/credential",
		CredentialToken: "cred-token",
	}
	if err := Sync(ctx, res, appCfg); err != nil {
		t.Fatalf("Sync (app): %v", err)
	}

	tokenCfg := appCfg
	tokenCfg.App = fakeApp{configured: false}
	if err := Sync(ctx, res, tokenCfg); err != nil {
		t.Fatalf("Sync (token): %v", err)
	}

	for _, key := range []string{"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", EnvCredentialURL, EnvCredentialToken} {
		if v := os.Getenv(key); v != "" {
			t.Errorf("%s = %q, want unset after falling back to the token", key, v)
		}
	}
	if v := os.Getenv("GIT_CONFIG_COUNT"); v != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 1", v)
	}
}
