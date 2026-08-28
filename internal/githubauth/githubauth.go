// Package githubauth makes the harness's GitHub credential ambient for
// every subprocess this process spawns — a clone the workspace runs
// (internal/workspace/clone.go), and any git or gh call an agent session
// runs from inside its own workspace (internal/tools/bash.go). Both spawn
// with a nil or os.Environ()-derived cmd.Env, so a value this package sets
// on the harness process's own environment reaches every one of them with
// no change at either call site.
//
// There are two credentials it can make ambient, and which one it picks is
// the whole of Sync's decision:
//
// A GitHub App (github.app_id and github.app_private_key), when both are
// set, wins. An App has no standing token — it mints one per account it is
// installed on, each good for an hour (internal/githubapp) — so there is
// nothing to write into a credential file. git is pointed at a credential
// helper instead: `harness github-credential`, which asks this process for
// the token belonging to the owner of the repository git is talking to.
// That indirection is what lets one credential cover a personal account and
// an organisation at once, which is what an App is for.
//
// A personal access token (github.token) is the fallback, and behaves as it
// always has: one token, written into a credential file git reads through
// the `store` helper, and exported as GITHUB_TOKEN and GH_TOKEN.
//
// Sync is deliberately not threaded through a resolver at each exec site.
// The alternative — passing the token to git and gh explicitly on every
// call — would mean plumbing a *settings.Resolver into internal/tools and
// internal/workspace, both of which are leaves today (ARCHITECTURE.md's
// codemap). Setting process environment variables instead reproduces
// exactly what scripts/docker-entrypoint.sh used to do from a fixed
// GITHUB_TOKEN env var, except driven by settings a running harness can
// change without a restart.
//
// It writes no global git config. Outside a container, harness serve runs
// as the operator, and $HOME/.gitconfig is theirs — a process editing it
// out from under them is exactly what the entrypoint's own comment used to
// warn against for the container case, and it applies with more force on a
// bare-metal install. GIT_CONFIG_COUNT / GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n carry the configuration through the process
// environment instead, which every child inherits and which vanishes with
// the process — nothing is left behind in a file outside the data
// directory.
package githubauth

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// credentialFileMode is the permission the credential file is written and
// expected to carry: readable and writable by the owner only. It holds a
// bearer credential in plain text, the same shape any git credential store
// file does.
const credentialFileMode = 0o600

// Environment variables the credential helper reads to find the harness it
// belongs to. git runs the helper as a bare subprocess with no arguments of
// our choosing beyond the ones in the config value, so the callback address
// and its bearer token travel the same way everything else here does: the
// process environment, inherited from the harness that set them.
const (
	EnvCredentialURL   = "HARNESS_GIT_CREDENTIAL_URL"
	EnvCredentialToken = "HARNESS_GIT_CREDENTIAL_TOKEN"
)

// AppProvider is the slice of *githubapp.Provider this package needs: only
// whether an App is configured. Minting tokens is the credential helper's
// business, not Sync's, and keeping the seam this narrow means the fallback
// decision can be tested without a GitHub stub.
type AppProvider interface {
	Configured(ctx context.Context) (bool, error)
}

// Config is everything Sync needs that is not a setting.
type Config struct {
	// CredentialPath is the file the personal-access-token path writes, and
	// the file the App path makes sure is absent. It lives under the data
	// directory (cmd/harness/serve.go).
	CredentialPath string

	// App reports whether a GitHub App is configured. Nil means no App is
	// possible in this process, and Sync always takes the token path — which
	// is what every caller that predates Apps gets by leaving it unset.
	App AppProvider

	// HelperCommand is the command line git runs to ask for a credential,
	// normally "<path to the harness binary> github-credential". Empty
	// disables the App path even when an App is configured: with no helper
	// to run, an App would leave git with no credential at all, and falling
	// back to github.token is strictly better than that.
	HelperCommand string

	// CredentialURL is where the helper calls back to reach this process's
	// POST /api/github/credential, and CredentialToken is the bearer token
	// that endpoint requires. Both are set by cmd/harness/serve.go once the
	// listen address is known.
	CredentialURL   string
	CredentialToken string
}

// Sync makes the configured GitHub credential the harness process's own git
// and gh credential. It is called once at startup and again after every
// write to one of the GitHub settings (cmd/harness/serve.go's
// httpapi.Server.OnSettingChanged), so a credential typed into the settings
// screen takes effect without a restart — the same "reads through on every
// call" contract the rest of internal/settings carries, applied to the
// settings that also have to reach outside this process's own address
// space.
//
// With a GitHub App configured and a helper command to run, it removes any
// credential file the token path left, points git at the helper through
// GIT_CONFIG_*, sets credential.useHttpPath so git tells the helper which
// repository it is asking about (without it there is no owner to route on),
// and publishes the callback address and token the helper needs. GITHUB_TOKEN
// and GH_TOKEN are unset: no one token is right for every account, so the
// Bash tool supplies gh a token per session instead
// (internal/tools.Executor.ExtraEnv, wired in cmd/harness/serve.go).
//
// Otherwise it takes the personal-access-token path. With a token set it
// writes CredentialPath at mode 0600 holding
// "https://x-access-token:<token>@github.com", points git at the `store`
// helper reading exactly that file, and sets GITHUB_TOKEN and GH_TOKEN —
// gh reads GH_TOKEN first and GITHUB_TOKEN second; git reads neither, which
// is what the credential file is for. With no token set it removes the file
// (a missing file is not an error) and unsets both variables, leaving the
// harness with no GitHub credential at all — the state a fresh install
// starts in, where private clones fail individually until an operator sets
// one.
func Sync(ctx context.Context, res *settings.Resolver, cfg Config) error {
	useApp := false
	if cfg.App != nil && cfg.HelperCommand != "" {
		configured, err := cfg.App.Configured(ctx)
		if err != nil {
			return err
		}
		useApp = configured
	}
	if useApp {
		return syncApp(cfg)
	}
	return syncToken(ctx, res, cfg)
}

// syncApp points git at the credential helper and clears every trace of the
// token path.
func syncApp(cfg Config) error {
	if err := os.Remove(cfg.CredentialPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("githubauth: remove %s: %w", cfg.CredentialPath, err)
	}
	if err := setEnv(map[string]string{
		"GITHUB_TOKEN":     "",
		"GH_TOKEN":         "",
		EnvCredentialURL:   cfg.CredentialURL,
		EnvCredentialToken: cfg.CredentialToken,
	}); err != nil {
		return err
	}
	// The leading "!" makes git run the value through a shell, which is how
	// a helper that is an absolute path plus an argument is spelled.
	// useHttpPath is what makes git send path=<owner>/<repo>.git with the
	// request; the helper has no other way to know which installation to
	// mint a token from.
	return setGitConfig([][2]string{
		{"credential.helper", "!" + cfg.HelperCommand},
		{"credential.useHttpPath", "true"},
	})
}

// syncToken writes (or removes) the personal access token's credential file
// and environment.
func syncToken(ctx context.Context, res *settings.Resolver, cfg Config) error {
	token, ok, err := res.Get(ctx, settings.KeyGitHubToken)
	if err != nil {
		return fmt.Errorf("githubauth: resolve %s: %w", settings.KeyGitHubToken, err)
	}
	env := map[string]string{
		EnvCredentialURL:   "",
		EnvCredentialToken: "",
	}
	if ok && token != "" {
		content := fmt.Sprintf("https://x-access-token:%s@github.com\n", token)
		if err := os.WriteFile(cfg.CredentialPath, []byte(content), credentialFileMode); err != nil {
			return fmt.Errorf("githubauth: write %s: %w", cfg.CredentialPath, err)
		}
		env["GITHUB_TOKEN"] = token
		env["GH_TOKEN"] = token
	} else {
		if err := os.Remove(cfg.CredentialPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("githubauth: remove %s: %w", cfg.CredentialPath, err)
		}
		env["GITHUB_TOKEN"] = ""
		env["GH_TOKEN"] = ""
	}
	if err := setEnv(env); err != nil {
		return err
	}
	return setGitConfig([][2]string{
		{"credential.helper", "store --file=" + cfg.CredentialPath},
	})
}

// setEnv sets each named variable, or unsets it when the value is empty, so
// a switch between the App and the token path never leaves the other one's
// variables behind for a subprocess to find.
func setEnv(values map[string]string) error {
	for key, value := range values {
		if value == "" {
			if err := os.Unsetenv(key); err != nil {
				return fmt.Errorf("githubauth: unset %s: %w", key, err)
			}
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("githubauth: set %s: %w", key, err)
		}
	}
	return nil
}

// setGitConfig publishes entries as git's GIT_CONFIG_COUNT / KEY_n / VALUE_n
// environment configuration. It unsets the two indices past the end as well:
// the App path writes two entries and the token path one, so a harness that
// switches from the first to the second would otherwise leave KEY_1 and
// VALUE_1 in the environment — dead while GIT_CONFIG_COUNT is 1, and alive
// again the moment anything raised it.
func setGitConfig(entries [][2]string) error {
	if err := os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(entries))); err != nil {
		return fmt.Errorf("githubauth: set GIT_CONFIG_COUNT: %w", err)
	}
	for i, entry := range entries {
		if err := os.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i), entry[0]); err != nil {
			return fmt.Errorf("githubauth: set GIT_CONFIG_KEY_%d: %w", i, err)
		}
		if err := os.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), entry[1]); err != nil {
			return fmt.Errorf("githubauth: set GIT_CONFIG_VALUE_%d: %w", i, err)
		}
	}
	for i := len(entries); i < len(entries)+2; i++ {
		os.Unsetenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		os.Unsetenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
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
