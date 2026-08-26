package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// "github repos"). A read-only, config-adjacent endpoint: it reads the
// operator's github.token setting and asks the GitHub REST API for the
// account's repositories, newest-updated first, so the start-run form can
// offer a searchable picker instead of requiring a repo URL typed from
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

// handleListGithubRepos serves GET /api/github/repos. With no github.token
// set it answers 200 {"repos": [], "configured": false} — the expected
// unconfigured state the form shows a quiet hint for. With a token set it
// returns the account's repos from the in-memory cache when fresh, or fetches
// them from GitHub (paginated, capped at githubMaxPages), caches them, and
// answers {"repos": [...], "configured": true}. A GitHub-side failure — a bad
// or expired token, a rate limit, a network error — is a 502 carrying a
// readable message, so the frontend can tell that state apart from "no token
// configured yet" and surface it as a quiet hint rather than blocking the
// form.
func (s *Server) handleListGithubRepos(w http.ResponseWriter, r *http.Request) {
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

// fetchGithubRepos walks the GitHub repo list endpoint, following
// Link: rel="next" pages, capped at githubMaxPages, and maps each page's rows
// to githubRepo. The query (sort=updated, direction=desc, per_page=100,
// affiliation=owner,collaborator,organization_member) is what makes GitHub
// return the account's own, collaborator, and org repos newest-first, so the
// caller gets the list already ordered without a server-side re-sort. An
// error names the GitHub side of the failure — the status and the API's own
// message for an HTTP refusal, the transport error for a network failure —
// because the frontend shows it verbatim.
func fetchGithubRepos(ctx context.Context, token, baseURL string) ([]githubRepo, error) {
	client := &http.Client{Timeout: githubTimeout}
	var repos []githubRepo
	pageURL := strings.TrimSuffix(baseURL, "/") +
		"/user/repos?sort=updated&direction=desc&per_page=100&affiliation=owner,collaborator,organization_member"
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
		var pageRepos []githubRepo
		if err := json.Unmarshal(body, &pageRepos); err != nil {
			return nil, fmt.Errorf("GitHub returned an unparseable repo list: %w", err)
		}
		repos = append(repos, pageRepos...)
		pageURL = nextPageLink(resp.Header.Get("Link"))
	}
	return repos, nil
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
