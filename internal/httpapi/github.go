package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// githubCache holds the last successful GET /api/github/repos fetch and its
// time — the only mutable state on an otherwise stateless Server. get and
// set are safe for concurrent use: two dialog-opens racing each other is the
// only contention this ever sees, and a lost update between them is
// harmless (two fetches of the same list), so the fetch itself runs outside
// the lock (handleListGithubRepos).
type githubCache struct {
	mu        sync.Mutex
	repos     []githubRepo
	fetchedAt time.Time
}

// get returns the cached repo list and whether it is still fresh (fetched
// less than githubCacheTTL ago). A never-populated cache — and a cache that
// holds an empty list — is a miss, so the first fetch after a 60-second
// silence re-hits GitHub.
func (c *githubCache) get() ([]githubRepo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repos != nil && time.Since(c.fetchedAt) < githubCacheTTL {
		return c.repos, true
	}
	return nil, false
}

// set stores a freshly fetched repo list and its fetch time.
func (c *githubCache) set(repos []githubRepo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repos = repos
	c.fetchedAt = time.Now()
}

// The GitHub repo list behind GET /api/github/repos (docs/DATA-API.md
// "github repos"), and the credential endpoint the git helper calls
// (docs/GITHUB-APP.md). The list is a read-only, config-adjacent endpoint:
// it reads the operator's GitHub credential and asks the GitHub REST API for
// the repositories it can see, newest-updated first, so the start-run form
// can offer a searchable picker instead of requiring a repo URL typed from
// memory. It needs no RunPublisher/RunController seam — those exist only for
// run-control write paths — and no write guards, because it is a GET with no
// side effects, exactly like handleGetSettings.

const (
	// githubDefaultBaseURL is the GitHub REST API root. Server.GitHubBaseURL
	// overrides it in tests, which stand in for GitHub with an httptest.Server.
	githubDefaultBaseURL = "https://api.github.com"

	// githubMaxPages caps the Link: rel="next" pagination walk at five
	// pages (500 repos at per_page=100), so one dialog-open cannot hang on
	// an account with thousands of repositories.
	githubMaxPages = 5

	// githubCacheTTL is how long a successful repo fetch stays cached on the
	// Server. Reopening the start-run dialog repeatedly re-reads the cache
	// instead of re-hitting GitHub's rate-limited API every time.
	githubCacheTTL = 60 * time.Second

	// githubTimeout is the http.Client's per-request timeout, so a hung
	// GitHub page cannot stall a dialog-open. It bounds one page, not the
	// whole walk: five pages of a healthy-but-slow API may take longer, but
	// the per_page=100 pages are small and the cap of githubMaxPages bounds
	// the total.
	githubTimeout = 10 * time.Second
)

// githubRepo is one row of GET /api/github/repos's repos array, the fields
// the start-run form's picker needs — the full name it searches, the clone
// URL and default branch it fills the repo row with, whether it is private,
// and when it was last updated. GitHub already returns the list sorted by
// updated_at desc per the query, so the mapping is field-for-field and the
// order is preserved.
type githubRepo struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	UpdatedAt     string `json:"updated_at"`
}

// githubReposResponse is GET /api/github/repos's 200 body. configured tells
// the frontend "no github.token set yet" apart from "token set but the
// GitHub call failed": false with an empty repos array is the expected
// unconfigured state, not an error.
type githubReposResponse struct {
	Repos      []githubRepo `json:"repos"`
	Configured bool         `json:"configured"`
}

// githubBaseURL returns the GitHub REST API base this Server talks to: the
// real api.github.com unless a test overrode Server.GitHubBaseURL.
func (s *Server) githubBaseURL() string {
	if s.GitHubBaseURL != "" {
		return s.GitHubBaseURL
	}
	return githubDefaultBaseURL
}

// handleListGithubRepos serves GET /api/github/repos. With no credential
// configured — neither a GitHub App nor github.token — it answers
// 200 {"repos": [], "configured": false}, the expected unconfigured state the
// form shows a quiet hint for. With one configured it returns the repos from
// the in-memory cache when fresh, or fetches them from GitHub (paginated,
// capped at githubMaxPages), caches them, and answers {"repos": [...],
// "configured": true}. A GitHub-side failure — a bad or expired credential, a
// rate limit, a network error — is a 502 carrying a readable message, so the
// frontend can tell that state apart from "nothing configured yet" and
// surface it as a quiet hint rather than blocking the form.
//
// The cache is shared by both paths, and it is keyed on nothing: an operator
// who switches credentials keeps seeing the old list until the 60 seconds
// are up. That is the same staleness the endpoint already accepted for a
// token replaced in place, and 60 seconds of it is cheaper than a cache key
// nothing else needs.
func (s *Server) handleListGithubRepos(w http.ResponseWriter, r *http.Request) {
	// A configured GitHub App answers this endpoint instead of the token,
	// and answers it better: the list is every installation's repositories
	// merged, so a personal account's repos and an organisation's appear in
	// one picker — the thing a single fine-grained token cannot do
	// (docs/GITHUB-APP.md).
	if s.GitHubApp != nil {
		configured, err := s.GitHubApp.Configured(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if configured {
			if repos, fresh := s.github.get(); fresh {
				writeJSON(w, http.StatusOK, githubReposResponse{Repos: repos, Configured: true})
				return
			}
			repos, err := s.fetchInstallationRepos(r.Context())
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			s.github.set(repos)
			writeJSON(w, http.StatusOK, githubReposResponse{Repos: repos, Configured: true})
			return
		}
	}

	token, ok, err := s.Settings.Get(r.Context(), settings.KeyGitHubToken)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, githubReposResponse{Repos: []githubRepo{}, Configured: false})
		return
	}

	if repos, fresh := s.github.get(); fresh {
		writeJSON(w, http.StatusOK, githubReposResponse{Repos: repos, Configured: true})
		return
	}

	repos, err := fetchGithubRepos(r.Context(), token, s.githubBaseURL())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.github.set(repos)
	writeJSON(w, http.StatusOK, githubReposResponse{Repos: repos, Configured: true})
}

// fetchGithubRepos lists the repositories a personal access token can see.
// The query (sort=updated, direction=desc, per_page=100,
// affiliation=owner,collaborator,organization_member) is what makes GitHub
// return the account's own, collaborator, and org repos newest-first, so the
// caller gets the list already ordered without a server-side re-sort. The
// walk itself — pagination, decoding, and the errors that name the GitHub
// side of a failure verbatim for the frontend — is fetchGithubReposFor,
// shared with the App path.
func fetchGithubRepos(ctx context.Context, token, baseURL string) ([]githubRepo, error) {
	return fetchGithubReposFor(ctx, token, strings.TrimSuffix(baseURL, "/")+
		"/user/repos?sort=updated&direction=desc&per_page=100&affiliation=owner,collaborator,organization_member")
}

// githubStatusError turns a non-200 GitHub response into the readable message
// the 502 carries. The GitHub API's own {"message": "..."} body names the
// reason; the status adds the shape — 401 is a bad or expired token, 403 a
// rate limit or scope refusal — so the operator knows which setting to fix
// without reading GitHub's docs.
func githubStatusError(status int, body []byte) error {
	message := http.StatusText(status)
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		message = parsed.Message
	}
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub rejected the token (401): %s — check github.token in Settings", message)
	case http.StatusForbidden:
		return fmt.Errorf("GitHub refused the request (403): %s", message)
	case http.StatusNotFound:
		return fmt.Errorf("GitHub answered 404: %s", message)
	default:
		return fmt.Errorf("GitHub answered %d: %s", status, message)
	}
}

// nextPageLink extracts the rel="next" URL from a Link response header, the
// pagination handshake GitHub documents: `<https://api.github.com/...>; rel="next",
// <https://api.github.com/...>; rel="last"`. An absent or malformed header
// returns "", which ends the walk at the current page.
func nextPageLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segments[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, param := range segments[1:] {
			if strings.TrimSpace(param) == `rel="next"` {
				return urlPart[1 : len(urlPart)-1]
			}
		}
	}
	return ""
}

// fetchInstallationRepos merges every installation's repository list into
// one, newest-updated first.
//
// Each installation needs its own token and its own walk — an installation
// token can only see the account it was minted for — so this is one fetch
// per account the App is installed on, and the merged list is re-sorted
// here because "newest first" across two accounts is not either account's
// own order. The cost is bounded by how many accounts an operator installs
// the App on, and the whole answer is cached for githubCacheTTL like the
// token path's is.
//
// One account failing fails the request. A partial list would be worse than
// an error here: the picker would quietly stop offering an organisation's
// repos, and the operator would have no way to tell that from the
// organisation having none.
func (s *Server) fetchInstallationRepos(ctx context.Context) ([]githubRepo, error) {
	owners, err := s.GitHubApp.Owners(ctx)
	if err != nil {
		return nil, err
	}
	var repos []githubRepo
	for _, owner := range owners {
		token, err := s.GitHubApp.TokenForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		owned, err := fetchGithubReposFor(ctx, token, s.githubBaseURL()+"/installation/repositories?per_page=100")
		if err != nil {
			return nil, fmt.Errorf("listing %s's repositories: %w", owner, err)
		}
		repos = append(repos, owned...)
	}
	// updated_at is RFC 3339 in UTC, so the lexical order is the
	// chronological one and no time parsing is needed to sort by it.
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].UpdatedAt > repos[j].UpdatedAt })
	if repos == nil {
		repos = []githubRepo{}
	}
	return repos, nil
}

// fetchGithubReposFor walks a repository listing endpoint from pageURL,
// following Link: rel="next" up to githubMaxPages, and decodes each page as
// either a bare array (/user/repos, the token path) or GitHub's
// {"repositories": [...]} envelope (/installation/repositories, the App
// path). Both shapes are tried rather than the caller declaring which,
// because the two endpoints differ in nothing else this function does.
func fetchGithubReposFor(ctx context.Context, token, pageURL string) ([]githubRepo, error) {
	client := &http.Client{Timeout: githubTimeout}
	var repos []githubRepo
	for page := 0; pageURL != "" && page < githubMaxPages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build GitHub request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		// GitHub rejects requests without a User-Agent, so one is always set.
		req.Header.Set("User-Agent", "deepseek-harness")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GitHub request failed: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read GitHub response: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, githubStatusError(resp.StatusCode, body)
		}
		pageRepos, err := decodeRepoPage(body)
		if err != nil {
			return nil, err
		}
		repos = append(repos, pageRepos...)
		pageURL = nextPageLink(resp.Header.Get("Link"))
	}
	return repos, nil
}

// decodeRepoPage reads one page of repositories in either of the two shapes
// GitHub serves them in.
func decodeRepoPage(body []byte) ([]githubRepo, error) {
	var bare []githubRepo
	if err := json.Unmarshal(body, &bare); err == nil {
		return bare, nil
	}
	var wrapped struct {
		Repositories []githubRepo `json:"repositories"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("GitHub returned an unparseable repo list: %w", err)
	}
	return wrapped.Repositories, nil
}

// handleGithubCredential serves POST /api/github/credential: the endpoint
// the `harness github-credential` git credential helper calls to turn a
// repository owner into a usable password (docs/GITHUB-APP.md, "How git
// gets a token").
//
// It exists because git needs a different installation token per owner and
// a fresh one every hour, and a credential *file* can express neither. The
// helper is a separate process — git spawns it — so the two have to talk
// over something; this process already serves HTTP, and it is the process
// holding the App's private key and the token cache, which is where both
// belong.
//
// The bearer token is its own, not http.control_token: every agent session
// runs as a child of this process and can read the environment the helper
// reads, so whatever guards this endpoint is effectively known to every
// session. That is acceptable for minting installation tokens — a session's
// own git can mint them anyway — and would not be acceptable for the
// run-control surface, which is why the two credentials are separate
// (cmd/harness/serve.go).
func (s *Server) handleGithubCredential(w http.ResponseWriter, r *http.Request) {
	if s.GitCredentialToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the git credential endpoint is not configured",
		})
		return
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) ||
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(s.GitCredentialToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
		return
	}
	var body struct {
		Host  string `json:"host"`
		Owner string `json:"owner"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unparseable body: " + err.Error()})
		return
	}
	if body.Host != "" && body.Host != "github.com" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "the GitHub App has no credential for " + body.Host,
		})
		return
	}
	if body.Owner == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner is required"})
		return
	}
	if s.GitHubApp == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no GitHub App is configured",
		})
		return
	}
	token, err := s.GitHubApp.TokenForOwner(r.Context(), body.Owner)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// x-access-token is the username GitHub documents for an installation
	// token over HTTPS; the token itself is the password.
	writeJSON(w, http.StatusOK, map[string]string{"username": "x-access-token", "password": token})
}

// GitHubApp is the slice of *githubapp.Provider this package uses: whether
// an App is configured at all, which accounts it is installed on, and a
// token for one of them. Declared here rather than imported so this package
// keeps no dependency on internal/githubapp — cmd/harness is where the two
// meet, as it is for every other seam on Server (ARCHITECTURE.md).
type GitHubApp interface {
	Configured(ctx context.Context) (bool, error)
	Owners(ctx context.Context) ([]string, error)
	TokenForOwner(ctx context.Context, owner string) (string, error)
}
