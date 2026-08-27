package githubauth

import (
	"context"
	"os"
	"path/filepath"
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
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		os.Unsetenv(key)
	}
	t.Cleanup(func() {
		for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
			os.Unsetenv(key)
		}
	})
}

func TestSyncWritesCredentialFileAndEnv(t *testing.T) {
	clearGithubEnv(t)
	res := settings.NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()
	credPath := filepath.Join(t.TempDir(), "git-credentials")

	if err := res.Set(ctx, settings.KeyGitHubToken, "ghp_abc123"); err != nil {
		t.Fatalf("Set github.token: %v", err)
	}
	if err := Sync(ctx, res, credPath); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat %s: %v", credPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file mode = %o, want 0600", perm)
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
	if err := Sync(ctx, res, credPath); err != nil {
		t.Fatalf("Sync (set): %v", err)
	}
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("credential file should exist after the first Sync: %v", err)
	}

	if err := res.Unset(ctx, settings.KeyGitHubToken); err != nil {
		t.Fatalf("Unset github.token: %v", err)
	}
	if err := Sync(ctx, res, credPath); err != nil {
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

	if err := Sync(ctx, res, credPath); err != nil {
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
