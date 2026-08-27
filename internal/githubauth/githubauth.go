// Package githubauth makes the stored github.token setting ambient for
// every subprocess this process spawns — a clone the workspace runs
// (internal/workspace/clone.go), and any git or gh call an agent session
// runs from inside its own workspace (internal/tools/bash.go). Both spawn
// with a nil or os.Environ()-derived cmd.Env, so a value this package sets
// on the harness process's own environment reaches every one of them with
// no change at either call site.
//
// Sync is deliberately not threaded through a resolver at each exec site.
// The alternative — passing the token to git and gh explicitly on every
// call — would mean plumbing a *settings.Resolver into internal/tools and
// internal/workspace, both of which are leaves today (ARCHITECTURE.md's
// codemap). Setting process environment variables instead reproduces
// exactly what scripts/docker-entrypoint.sh used to do from a fixed
// GITHUB_TOKEN env var, except driven by a setting a running harness can
// change without a restart.
//
// It writes no global git config. Outside a container, harness serve runs
// as the operator, and $HOME/.gitconfig is theirs — a process editing it
// out from under them is exactly what the entrypoint's own comment used to
// warn against for the container case, and it applies with more force on a
// bare-metal install. GIT_CONFIG_COUNT / GIT_CONFIG_KEY_0 /
// GIT_CONFIG_VALUE_0 point git at the credential file through the process
// environment instead, which every child inherits and which vanishes with
// the process — nothing is left behind in a file outside the data
// directory.
package githubauth

import (
	"context"
	"fmt"
	"os"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// credentialFileMode is the permission the credential file is written and
// expected to carry: readable and writable by the owner only. It holds a
// bearer credential in plain text, the same shape any git credential store
// file does.
const credentialFileMode = 0o600

// Sync reads the stored github.token setting and makes it the harness
// process's own git and gh credential, or removes it when the setting is
// unset. It is called once at startup and again after every write to
// github.token (cmd/harness/serve.go's httpapi.Server.OnSettingChanged), so
// a token typed into the settings screen takes effect without a restart —
// the same "reads through on every call" contract the rest of
// internal/settings carries, applied to the one setting that also has to
// reach outside this process's own address space.
//
// With a token set, it writes credPath at mode 0600 holding
// "https://x-access-token:<token>@github.com", and sets GITHUB_TOKEN and
// GH_TOKEN in the process environment to the token — gh reads GH_TOKEN
// first and GITHUB_TOKEN second; git reads neither, which is what the
// credential file is for. With no token set, it removes credPath (a
// missing file is not an error) and unsets both variables. Either way it
// sets GIT_CONFIG_COUNT=1, GIT_CONFIG_KEY_0=credential.helper,
// GIT_CONFIG_VALUE_0="store --file=<credPath>" in the process environment,
// so git reads credentials from exactly this file rather than from any
// global config — see the package doc comment for why that is a process
// environment variable and not a `git config --global` call.
func Sync(ctx context.Context, res *settings.Resolver, credPath string) error {
	token, ok, err := res.Get(ctx, settings.KeyGitHubToken)
	if err != nil {
		return fmt.Errorf("githubauth: resolve %s: %w", settings.KeyGitHubToken, err)
	}
	if ok && token != "" {
		content := fmt.Sprintf("https://x-access-token:%s@github.com\n", token)
		if err := os.WriteFile(credPath, []byte(content), credentialFileMode); err != nil {
			return fmt.Errorf("githubauth: write %s: %w", credPath, err)
		}
		if err := os.Setenv("GITHUB_TOKEN", token); err != nil {
			return fmt.Errorf("githubauth: set GITHUB_TOKEN: %w", err)
		}
		if err := os.Setenv("GH_TOKEN", token); err != nil {
			return fmt.Errorf("githubauth: set GH_TOKEN: %w", err)
		}
	} else {
		if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("githubauth: remove %s: %w", credPath, err)
		}
		if err := os.Unsetenv("GITHUB_TOKEN"); err != nil {
			return fmt.Errorf("githubauth: unset GITHUB_TOKEN: %w", err)
		}
		if err := os.Unsetenv("GH_TOKEN"); err != nil {
			return fmt.Errorf("githubauth: unset GH_TOKEN: %w", err)
		}
	}
	if err := os.Setenv("GIT_CONFIG_COUNT", "1"); err != nil {
		return fmt.Errorf("githubauth: set GIT_CONFIG_COUNT: %w", err)
	}
	if err := os.Setenv("GIT_CONFIG_KEY_0", "credential.helper"); err != nil {
		return fmt.Errorf("githubauth: set GIT_CONFIG_KEY_0: %w", err)
	}
	if err := os.Setenv("GIT_CONFIG_VALUE_0", "store --file="+credPath); err != nil {
		return fmt.Errorf("githubauth: set GIT_CONFIG_VALUE_0: %w", err)
	}
	return nil
}

// SeedFromEnv migrates an existing install through the upgrade: when
// github.token is unset in the store and GITHUB_TOKEN is present in the
// process environment — the shape .env plus docker-compose.yml's env_file
// used to produce — it stores that value and reports true, so a running
// install keeps working without the operator having to visit the settings
// screen first. It never overwrites a token already stored: an operator who
// has already moved to the settings screen, including one who cleared the
// setting on purpose, is not overridden by an old .env.
//
// The operator is meant to delete GITHUB_TOKEN from .env once the seed has
// run; leaving it there is harmless until the setting is unset again — an
// unset github.token with GITHUB_TOKEN still in the environment seeds right
// back in on the next start.
func SeedFromEnv(ctx context.Context, res *settings.Resolver) (bool, error) {
	_, ok, err := res.Get(ctx, settings.KeyGitHubToken)
	if err != nil {
		return false, fmt.Errorf("githubauth: resolve %s: %w", settings.KeyGitHubToken, err)
	}
	if ok {
		return false, nil
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return false, nil
	}
	if err := res.Set(ctx, settings.KeyGitHubToken, token); err != nil {
		return false, fmt.Errorf("githubauth: seed %s from GITHUB_TOKEN: %w", settings.KeyGitHubToken, err)
	}
	return true, nil
}
