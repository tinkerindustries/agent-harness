// Package githubapp authenticates the harness as a GitHub App rather than
// as one account's personal access token.
//
// The problem it exists for is that a fine-grained PAT belongs to exactly
// one account. An operator whose work spans a personal account and an
// organisation had to choose which one the harness could reach. A GitHub
// App is installed separately on each account, so one App — one id, one
// private key — covers both.
//
// What that costs is that an App has no single standing credential. It
// signs a JWT with its private key (jwt.go), asks GitHub which accounts it
// is installed on, and exchanges the JWT for an *installation* token per
// account, each valid for an hour and each usable only on the repositories
// of the account it was minted for. Every caller therefore has to say which
// owner it wants a token for, and tokens have to be re-minted as they
// expire. Provider is where both of those live: TokenForOwner is the whole
// interface the rest of the harness sees, and the caching behind it is this
// package's business.
//
// Where the tokens go: internal/githubauth points git at a credential
// helper that asks for one per repository (the helper is `harness
// github-credential`, which calls back into this process's HTTP API), and
// the Bash tool puts one in GH_TOKEN for the session's own repositories.
// GET /api/github/repos lists every installation's repositories through it
// too, which is why the start-run picker shows a personal account's repos
// and an organisation's in one list. docs/GITHUB-APP.md is the operator's
// side of all of it.
package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

const (
	// DefaultBaseURL is the GitHub REST API root. Provider.BaseURL overrides
	// it in tests, which stand in for GitHub with an httptest.Server.
	DefaultBaseURL = "https://api.github.com"

	// requestTimeout bounds one call to GitHub. Both calls this package
	// makes sit in front of a git operation waiting on a credential, so a
	// hung GitHub cannot be allowed to hang a push indefinitely.
	requestTimeout = 15 * time.Second

	// installationsTTL is how long the account-login-to-installation-id map
	// is trusted. Installing the App on a third account should not need a
	// harness restart, and a miss re-reads immediately anyway (see
	// installationFor), so this only bounds how long a *removed*
	// installation lingers.
	installationsTTL = 5 * time.Minute

	// tokenSkew is how far before its stated expiry an installation token is
	// treated as spent. GitHub issues them for an hour; handing one to git
	// with four seconds left would fail the push it was minted for.
	tokenSkew = 2 * time.Minute

	// userAgent is sent on every request. GitHub rejects requests without
	// one.
	userAgent = "deepseek-harness"
)

// Installation is one account the App is installed on: the id token
// exchange needs, and the account login the harness routes by (the owner
// segment of a repository's URL).
type Installation struct {
	ID    int64
	Login string
}

// cachedToken is one installation's live access token and the moment it
// stops being usable (GitHub's expires_at, less tokenSkew).
type cachedToken struct {
	token   string
	expires time.Time
}

// Provider mints installation tokens for the configured App, caching what
// it learns. Safe for concurrent use: several sessions push at once, and
// the credential helper's callbacks arrive on the HTTP server's own
// goroutines.
//
// The lock is held across the GitHub call rather than only around the map,
// which serialises two owners' first token mints behind each other. That is
// deliberate: the alternative is a per-owner lock table to save a few
// hundred milliseconds on a cold cache, and every call after the first is a
// map read for the next hour. requestTimeout bounds how long the lock can
// be held.
type Provider struct {
	// Settings is where github.app_id and github.app_private_key are read
	// from, on every call, so an App configured from the settings screen
	// takes effect without a restart — the same read-through contract the
	// rest of internal/settings carries.
	Settings *settings.Resolver

	// BaseURL overrides the GitHub REST API root. Empty means the real
	// api.github.com.
	BaseURL string

	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time

	mu           sync.Mutex
	installs     map[string]int64
	installsRead time.Time
	tokens       map[int64]cachedToken
}

// New returns a Provider reading its credentials through res.
func New(res *settings.Resolver) *Provider {
	return &Provider{Settings: res}
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) baseURL() string {
	if p.BaseURL != "" {
		return strings.TrimSuffix(p.BaseURL, "/")
	}
	return DefaultBaseURL
}

// Configured reports whether both halves of the App credential are stored.
// One half alone is not a usable App and is treated as no App at all, so
// the harness falls back to github.token rather than failing every git
// operation while an operator is half way through filling the screen in.
func (p *Provider) Configured(ctx context.Context) (bool, error) {
	if p == nil || p.Settings == nil {
		return false, nil
	}
	appID, err := p.Settings.String(ctx, settings.KeyGitHubAppID)
	if err != nil {
		return false, fmt.Errorf("githubapp: resolve %s: %w", settings.KeyGitHubAppID, err)
	}
	key, err := p.Settings.String(ctx, settings.KeyGitHubAppPrivateKey)
	if err != nil {
		return false, fmt.Errorf("githubapp: resolve %s: %w", settings.KeyGitHubAppPrivateKey, err)
	}
	return strings.TrimSpace(appID) != "" && strings.TrimSpace(key) != "", nil
}

// jwt reads the stored credential and signs a fresh app JWT with it.
func (p *Provider) jwt(ctx context.Context) (string, error) {
	appID, err := p.Settings.String(ctx, settings.KeyGitHubAppID)
	if err != nil {
		return "", fmt.Errorf("githubapp: resolve %s: %w", settings.KeyGitHubAppID, err)
	}
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return "", fmt.Errorf("githubapp: %s is not set", settings.KeyGitHubAppID)
	}
	if _, err := strconv.ParseInt(appID, 10, 64); err != nil {
		return "", fmt.Errorf("githubapp: %s is %q, which is not the numeric App id from the App's settings page", settings.KeyGitHubAppID, appID)
	}
	pem, err := p.Settings.String(ctx, settings.KeyGitHubAppPrivateKey)
	if err != nil {
		return "", fmt.Errorf("githubapp: resolve %s: %w", settings.KeyGitHubAppPrivateKey, err)
	}
	key, err := ParsePrivateKey(pem)
	if err != nil {
		return "", err
	}
	return signJWT(key, appID, p.now())
}

// Installations returns every account the App is installed on, refreshing
// the cache when it has gone stale. The order is GitHub's own.
func (p *Provider) Installations(ctx context.Context) ([]Installation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.refreshInstallations(ctx, false); err != nil {
		return nil, err
	}
	out := make([]Installation, 0, len(p.installs))
	for login, id := range p.installs {
		out = append(out, Installation{ID: id, Login: login})
	}
	return out, nil
}

// Owners returns the account logins the App is installed on, lowercased —
// the form TokenForOwner takes. It is what GET /api/github/repos walks to
// build one repository list out of several installations.
func (p *Provider) Owners(ctx context.Context) ([]string, error) {
	installs, err := p.Installations(ctx)
	if err != nil {
		return nil, err
	}
	logins := make([]string, 0, len(installs))
	for _, install := range installs {
		logins = append(logins, install.Login)
	}
	sort.Strings(logins)
	return logins, nil
}

// TokenForOwner returns an installation access token valid for owner's
// repositories — the password half of the credential git and gh use.
//
// A cached token is returned until it is within tokenSkew of expiry. An
// owner the App is not installed on is an error naming the owner and the
// accounts it is installed on, because that message is what an operator
// sees when a clone fails, and "not installed there" and "the credential is
// wrong" need telling apart at a glance.
func (p *Provider) TokenForOwner(ctx context.Context, owner string) (string, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" {
		return "", fmt.Errorf("githubapp: no repository owner to mint a token for")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	id, err := p.installationFor(ctx, owner)
	if err != nil {
		return "", err
	}
	if tok, ok := p.tokens[id]; ok && p.now().Before(tok.expires) {
		return tok.token, nil
	}
	tok, err := p.mintToken(ctx, id)
	if err != nil {
		return "", err
	}
	if p.tokens == nil {
		p.tokens = make(map[int64]cachedToken)
	}
	p.tokens[id] = tok
	return tok.token, nil
}

// installationFor resolves an owner login to its installation id. A miss
// forces one refresh before it gives up: the App having just been installed
// on a new account is the expected reason for a miss, and waiting out
// installationsTTL for the first clone to work would be a puzzling few
// minutes. Callers hold p.mu.
func (p *Provider) installationFor(ctx context.Context, owner string) (int64, error) {
	if err := p.refreshInstallations(ctx, false); err != nil {
		return 0, err
	}
	if id, ok := p.installs[owner]; ok {
		return id, nil
	}
	if err := p.refreshInstallations(ctx, true); err != nil {
		return 0, err
	}
	if id, ok := p.installs[owner]; ok {
		return id, nil
	}
	known := make([]string, 0, len(p.installs))
	for login := range p.installs {
		known = append(known, login)
	}
	if len(known) == 0 {
		return 0, fmt.Errorf("githubapp: the GitHub App is not installed on any account; install it on %s at https://github.com/settings/installations", owner)
	}
	return 0, fmt.Errorf("githubapp: the GitHub App is not installed on %q (it is installed on %s); install it there too", owner, strings.Join(known, ", "))
}

// refreshInstallations reloads the login-to-id map when it is stale, or
// unconditionally when force is set. Callers hold p.mu.
func (p *Provider) refreshInstallations(ctx context.Context, force bool) error {
	if !force && p.installs != nil && p.now().Sub(p.installsRead) < installationsTTL {
		return nil
	}
	jwt, err := p.jwt(ctx)
	if err != nil {
		return err
	}
	var page []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := p.call(ctx, http.MethodGet, p.baseURL()+"/app/installations?per_page=100", jwt, &page); err != nil {
		return err
	}
	installs := make(map[string]int64, len(page))
	for _, row := range page {
		if row.Account.Login == "" {
			continue
		}
		installs[strings.ToLower(row.Account.Login)] = row.ID
	}
	p.installs = installs
	p.installsRead = p.now()
	return nil
}

// mintToken exchanges the app JWT for one installation's access token.
// Callers hold p.mu.
func (p *Provider) mintToken(ctx context.Context, id int64) (cachedToken, error) {
	jwt, err := p.jwt(ctx)
	if err != nil {
		return cachedToken{}, err
	}
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", p.baseURL(), id)
	if err := p.call(ctx, http.MethodPost, endpoint, jwt, &body); err != nil {
		return cachedToken{}, err
	}
	if body.Token == "" {
		return cachedToken{}, fmt.Errorf("githubapp: GitHub returned an installation token with no token in it")
	}
	expires := body.ExpiresAt.Add(-tokenSkew)
	if body.ExpiresAt.IsZero() {
		// GitHub always sends expires_at; if a future version stops, an hour
		// from now is what the documented lifetime says, and the skew still
		// applies.
		expires = p.now().Add(time.Hour - tokenSkew)
	}
	return cachedToken{token: body.Token, expires: expires}, nil
}

// call performs one authenticated request against the GitHub API and
// decodes its JSON body into out. The error message names GitHub's own
// {"message": ...} where there is one: it reaches an operator through a
// failed git push or the start-run form, and "Bad credentials" says more
// about which setting to fix than a bare 401 does.
func (p *Provider) call(ctx context.Context, method, endpoint, jwt string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return fmt.Errorf("githubapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("githubapp: %s %s: %w", method, redactedPath(endpoint), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("githubapp: read response from %s: %w", redactedPath(endpoint), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("githubapp: GitHub returned an unparseable body from %s: %w", redactedPath(endpoint), err)
	}
	return nil
}

// statusError turns a refused GitHub call into the message an operator
// reads. 401 means the App id and the private key disagree with each other
// or with GitHub; 404 on an installation endpoint means the installation is
// gone. Both are configuration, and both are worth naming.
func statusError(status int, body []byte) error {
	message := http.StatusText(status)
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		message = parsed.Message
	}
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("githubapp: GitHub rejected the App credential (401): %s — check %s and %s in Settings", message, settings.KeyGitHubAppID, settings.KeyGitHubAppPrivateKey)
	case http.StatusNotFound:
		return fmt.Errorf("githubapp: GitHub answered 404: %s — the App may have been uninstalled from that account", message)
	default:
		return fmt.Errorf("githubapp: GitHub answered %d: %s", status, message)
	}
}

// redactedPath is the endpoint without its query, for an error message. No
// credential is ever in a URL here, but an error string is a surface that
// reaches a transcript, and a path is all the reader needs.
func redactedPath(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	u.RawQuery = ""
	return u.String()
}
