package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// newGithubTestServer builds a Server like newTestServer but with
// GitHubBaseURL pointed at gh, the httptest.Server standing in for the
// GitHub REST API. token, when non-empty, is stored as github.token first, so
// the handler's configured and unconfigured paths both have a real setup.
func newGithubTestServer(t *testing.T, gh *httptest.Server, token string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if token != "" {
		if err := st.SetSetting(context.Background(), settings.KeyGitHubToken, token); err != nil {
			t.Fatalf("set github.token: %v", err)
		}
	}
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		GitHubBaseURL: gh.URL,
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "static placeholder")
		}),
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// githubRepoRow builds one GitHub API repo row in the wire shape the real
// /user/repos returns, so a fake page's payload matches what the mapper
// parses.
func githubRepoRow(fullName string) map[string]any {
	return map[string]any{
		"full_name":      fullName,
		"clone_url":      "https://github.com/" + fullName + ".git",
		"default_branch": "main",
		"private":        false,
		"updated_at":     "2026-01-02T03:04:05Z",
	}
}

// --- nextPageLink ---

func TestNextPageLink(t *testing.T) {
	cases := []struct {
		name string
		link string
		want string
	}{
		{
			name: "one next link",
			link: `<https://api.github.com/user/repos?page=2>; rel="next"`,
			want: "https://api.github.com/user/repos?page=2",
		},
		{
			name: "next among first and last",
			link: `<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=5>; rel="last"`,
			want: "https://api.github.com/user/repos?page=2",
		},
		{
			name: "empty header",
			link: "",
			want: "",
		},
		{
			name: "no next rel",
			link: `<https://api.github.com/user/repos?page=1>; rel="first", <https://api.github.com/user/repos?page=5>; rel="last"`,
			want: "",
		},
		{
			name: "malformed segment",
			link: `not-a-link; rel="next"`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextPageLink(tc.link); got != tc.want {
				t.Errorf("nextPageLink(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

// --- fetchGithubRepos ---

func TestFetchGithubReposPaginatesAndMapsFields(t *testing.T) {
	// A two-page fake: page 1 points at page 2 via Link: rel="next", page 2
	// has no next link. The walk must follow the header, fetch both pages,
	// and return the rows in wire order.
	var hits atomic.Int32
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/user/repos" {
			t.Errorf("path = %q, want /user/repos", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want application/vnd.github+json", got)
		}
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Error("User-Agent header is empty; GitHub rejects requests without one")
		}
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", `<`+gh.URL+`/user/repos?page=2>; rel="next"`)
			writeTestJSON(w, []map[string]any{githubRepoRow("org/alpha"), githubRepoRow("org/beta")})
		case "2":
			writeTestJSON(w, []map[string]any{githubRepoRow("org/gamma")})
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer gh.Close()

	repos, err := fetchGithubRepos(context.Background(), "test-token", gh.URL)
	if err != nil {
		t.Fatalf("fetchGithubRepos: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("GitHub hit %d times, want 2", hits.Load())
	}
	if len(repos) != 3 {
		t.Fatalf("got %d repos, want 3", len(repos))
	}
	// Wire order preserved: page 1's rows, then page 2's.
	want := []githubRepo{
		{FullName: "org/alpha", CloneURL: "https://github.com/org/alpha.git", DefaultBranch: "main", Private: false, UpdatedAt: "2026-01-02T03:04:05Z"},
		{FullName: "org/beta", CloneURL: "https://github.com/org/beta.git", DefaultBranch: "main", Private: false, UpdatedAt: "2026-01-02T03:04:05Z"},
		{FullName: "org/gamma", CloneURL: "https://github.com/org/gamma.git", DefaultBranch: "main", Private: false, UpdatedAt: "2026-01-02T03:04:05Z"},
	}
	for i, got := range repos {
		if got != want[i] {
			t.Errorf("repo %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestFetchGithubReposStopsAtPageCap(t *testing.T) {
	// Every page points at the next, forever; the walk must stop after
	// githubMaxPages pages rather than looping.
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := fmt.Sprintf("<%s/user/repos?page=%d>; rel=\"next\"", gh.URL, atoiOr(r.URL.Query().Get("page"), 1)+1)
		w.Header().Set("Link", next)
		writeTestJSON(w, []map[string]any{githubRepoRow("org/repo")})
	}))
	defer gh.Close()

	repos, err := fetchGithubRepos(context.Background(), "tok", gh.URL)
	if err != nil {
		t.Fatalf("fetchGithubRepos: %v", err)
	}
	if len(repos) != githubMaxPages {
		t.Fatalf("got %d repos, want the %d-page cap", len(repos), githubMaxPages)
	}
}

func TestFetchGithubReposSurfacesTokenRejection(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSONWithStatus(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
	}))
	defer gh.Close()

	_, err := fetchGithubRepos(context.Background(), "bad-token", gh.URL)
	if err == nil {
		t.Fatal("fetchGithubRepos succeeded, want an error for a rejected token")
	}
	for _, want := range []string{"401", "github.token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestFetchGithubReposRejectsUnparseablePage(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer gh.Close()

	if _, err := fetchGithubRepos(context.Background(), "tok", gh.URL); err == nil {
		t.Fatal("fetchGithubRepos succeeded, want an error for an unparseable page")
	}
}

func TestFetchGithubReposSurfacesNetworkFailure(t *testing.T) {
	// A server that closes the connection without answering, so client.Do
	// fails at the transport layer rather than with a status.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("connection dropped")
	}))
	url := gh.URL
	gh.Close()

	if _, err := fetchGithubRepos(context.Background(), "tok", url); err == nil {
		t.Fatal("fetchGithubRepos succeeded, want a network error")
	}
}

// --- handler ---

func TestGithubReposUnconfiguredReturnsEmptyList(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("GitHub must not be called when no token is set")
	}))
	defer gh.Close()
	srv := newGithubTestServer(t, gh, "")

	resp, err := http.Get(srv.URL + "/api/github/repos")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	var body githubReposResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Configured {
		t.Fatalf("configured = true, want false with no token: %+v", body)
	}
	if body.Repos == nil || len(body.Repos) != 0 {
		t.Fatalf("repos = %+v, want an empty array", body.Repos)
	}
}

func TestGithubReposConfiguredFetchesAndMaps(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, []map[string]any{
			githubRepoRow("org/alpha"),
			{"full_name": "user/private", "clone_url": "https://github.com/user/private.git", "default_branch": "dev", "private": true, "updated_at": "2026-02-03T04:05:06Z"},
		})
	}))
	defer gh.Close()
	srv := newGithubTestServer(t, gh, "test-token")

	resp, err := http.Get(srv.URL + "/api/github/repos")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, body)
	}
	var body githubReposResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Configured {
		t.Fatalf("configured = false, want true with a token: %+v", body)
	}
	if len(body.Repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(body.Repos))
	}
	if body.Repos[1].FullName != "user/private" || !body.Repos[1].Private || body.Repos[1].DefaultBranch != "dev" {
		t.Errorf("private repo row = %+v, want full_name user/private, private, branch dev", body.Repos[1])
	}
}

func TestGithubReposCachesForTTL(t *testing.T) {
	var hits atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeTestJSON(w, []map[string]any{githubRepoRow("org/alpha")})
	}))
	defer gh.Close()
	srv := newGithubTestServer(t, gh, "test-token")

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/api/github/repos")
		if err != nil {
			t.Fatal(err)
		}
		var body githubReposResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !body.Configured || len(body.Repos) != 1 {
			t.Fatalf("request %d: %+v, want one configured repo", i, body)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("GitHub hit %d times across 3 requests, want 1 (cached)", hits.Load())
	}
}

func TestGithubReposSurfacesGitHubFailureAs502(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSONWithStatus(w, http.StatusForbidden, map[string]any{"message": "API rate limit exceeded"})
	}))
	defer gh.Close()
	srv := newGithubTestServer(t, gh, "test-token")

	resp, err := http.Get(srv.URL + "/api/github/repos")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got status %d, want 502", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["error"], "rate limit") {
		t.Fatalf("error %q does not carry the GitHub message", body["error"])
	}
}

func TestGithubReposIsReadOnly(t *testing.T) {
	gh := httptest.NewServer(http.NotFoundHandler())
	defer gh.Close()
	srv := newGithubTestServer(t, gh, "")
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(method, srv.URL+"/api/github/repos", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/github/repos: got status %d, want 405", method, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s: Allow = %q, want \"GET, HEAD\"", method, got)
		}
	}
}

// writeTestJSON writes v as a 200 JSON body to w, the way the GitHub API
// would.
func writeTestJSON(w http.ResponseWriter, v any) {
	writeTestJSONWithStatus(w, http.StatusOK, v)
}

func writeTestJSONWithStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// atoiOr parses s as an int, returning def when it is empty or malformed —
// the tiny parse the page-cap test's "next page" Link needs.
func atoiOr(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}
