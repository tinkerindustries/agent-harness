package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// fakeGitHubApp stands in for *githubapp.Provider across the two endpoints
// that use it, with a token per owner and an error for anything else.
type fakeGitHubApp struct {
	configured bool
	tokens     map[string]string
	err        error
}

func (f fakeGitHubApp) Configured(ctx context.Context) (bool, error) { return f.configured, f.err }

func (f fakeGitHubApp) Owners(ctx context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	owners := make([]string, 0, len(f.tokens))
	for owner := range f.tokens {
		owners = append(owners, owner)
	}
	// Deterministic order, so a test can pin which installation is walked
	// first without depending on map iteration.
	for i := range owners {
		for j := i + 1; j < len(owners); j++ {
			if owners[j] < owners[i] {
				owners[i], owners[j] = owners[j], owners[i]
			}
		}
	}
	return owners, nil
}

func (f fakeGitHubApp) TokenForOwner(ctx context.Context, owner string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	token, ok := f.tokens[strings.ToLower(owner)]
	if !ok {
		return "", fmt.Errorf("the GitHub App is not installed on %q", owner)
	}
	return token, nil
}

// newGithubAppTestServer is newGithubTestServer with an App wired instead of
// a token, plus the credential endpoint's own bearer token.
func newGithubAppTestServer(t *testing.T, gh *httptest.Server, app GitHubApp, credentialToken string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		GitHubApp:          app,
		GitCredentialToken: credentialToken,
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "static placeholder")
		}),
	}
	if gh != nil {
		api.GitHubBaseURL = gh.URL
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postCredential(t *testing.T, srv *httptest.Server, bearer, body string) (*http.Response, map[string]string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/github/credential", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/github/credential: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var parsed map[string]string
	json.NewDecoder(resp.Body).Decode(&parsed)
	return resp, parsed
}

// TestGithubCredentialMintsForTheOwnerAsked is the endpoint's whole job: the
// git credential helper names an owner, and gets that installation's token
// back as the password git will use.
func TestGithubCredentialMintsForTheOwnerAsked(t *testing.T) {
	app := fakeGitHubApp{configured: true, tokens: map[string]string{
		"mrgeoffrich": "ghs_personal",
		"someorg":     "ghs_org",
	}}
	srv := newGithubAppTestServer(t, nil, app, "cred-token")

	resp, body := postCredential(t, srv, "cred-token", `{"host":"github.com","owner":"someorg"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["username"] != "x-access-token" {
		t.Errorf("username = %q, want x-access-token", body["username"])
	}
	if body["password"] != "ghs_org" {
		t.Errorf("password = %q, want the org's installation token", body["password"])
	}
}

// TestGithubCredentialRefusesAWrongOrMissingBearer pins that the endpoint is
// not open: it hands out live credentials, and the harness may be bound off
// loopback.
func TestGithubCredentialRefusesAWrongOrMissingBearer(t *testing.T) {
	app := fakeGitHubApp{configured: true, tokens: map[string]string{"mrgeoffrich": "ghs_personal"}}
	srv := newGithubAppTestServer(t, nil, app, "cred-token")

	for name, bearer := range map[string]string{"missing": "", "wrong": "not-the-token"} {
		t.Run(name, func(t *testing.T) {
			resp, _ := postCredential(t, srv, bearer, `{"owner":"mrgeoffrich"}`)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestGithubCredentialFailsClosedWithoutAToken pins that a Server built with
// no credential token — every caller that never configured one — refuses
// rather than serving credentials to anybody who asks.
func TestGithubCredentialFailsClosedWithoutAToken(t *testing.T) {
	app := fakeGitHubApp{configured: true, tokens: map[string]string{"mrgeoffrich": "ghs_personal"}}
	srv := newGithubAppTestServer(t, nil, app, "")

	resp, _ := postCredential(t, srv, "anything", `{"owner":"mrgeoffrich"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestGithubCredentialRejectsAnotherHost pins that the endpoint answers for
// github.com only: git asks every helper about every host, and this one has
// nothing to say about the rest.
func TestGithubCredentialRejectsAnotherHost(t *testing.T) {
	app := fakeGitHubApp{configured: true, tokens: map[string]string{"mrgeoffrich": "ghs_personal"}}
	srv := newGithubAppTestServer(t, nil, app, "cred-token")

	resp, _ := postCredential(t, srv, "cred-token", `{"host":"gitlab.com","owner":"mrgeoffrich"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestGithubCredentialCarriesTheAppsOwnError pins that "the App is not
// installed on that account" reaches git — and so the operator — instead of
// a bare status.
func TestGithubCredentialCarriesTheAppsOwnError(t *testing.T) {
	app := fakeGitHubApp{configured: true, tokens: map[string]string{"mrgeoffrich": "ghs_personal"}}
	srv := newGithubAppTestServer(t, nil, app, "cred-token")

	resp, body := postCredential(t, srv, "cred-token", `{"owner":"someoneelse"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(body["error"], "someoneelse") {
		t.Errorf("error = %q, want it to name the account", body["error"])
	}
}

// TestListGithubReposMergesInstallations is what the App buys the start-run
// form: one picker holding a personal account's repositories and an
// organisation's, newest-updated first across both — which is the thing a
// single fine-grained token cannot produce.
func TestListGithubReposMergesInstallations(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/installation/repositories" {
			http.Error(w, `{"message":"unexpected path"}`, http.StatusNotFound)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer ghs_personal":
			json.NewEncoder(w).Encode(map[string]any{"repositories": []map[string]any{
				githubRepoRowAt("mrgeoffrich/notes", "2026-01-01T00:00:00Z"),
			}})
		case "Bearer ghs_org":
			json.NewEncoder(w).Encode(map[string]any{"repositories": []map[string]any{
				githubRepoRowAt("someorg/app", "2026-06-01T00:00:00Z"),
			}})
		default:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		}
	}))
	defer gh.Close()

	app := fakeGitHubApp{configured: true, tokens: map[string]string{
		"mrgeoffrich": "ghs_personal",
		"someorg":     "ghs_org",
	}}
	srv := newGithubAppTestServer(t, gh, app, "cred-token")

	resp, err := http.Get(srv.URL + "/api/github/repos")
	if err != nil {
		t.Fatalf("GET /api/github/repos: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Repos []struct {
			FullName string `json:"full_name"`
		} `json:"repos"`
		Configured bool `json:"configured"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Configured {
		t.Error("configured = false with an App wired")
	}
	if len(body.Repos) != 2 {
		t.Fatalf("got %d repos, want both installations' repositories", len(body.Repos))
	}
	if body.Repos[0].FullName != "someorg/app" {
		t.Errorf("first repo = %q, want the most recently updated one across both accounts", body.Repos[0].FullName)
	}
}

// TestListGithubReposFallsBackToTheTokenWhenNoAppIsConfigured pins that
// wiring an App provider that reports itself unconfigured changes nothing:
// the token path answers exactly as it did before Apps existed.
func TestListGithubReposFallsBackToTheTokenWhenNoAppIsConfigured(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/user/repos") {
			http.Error(w, `{"message":"unexpected path"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{githubRepoRow("mrgeoffrich/notes")})
	}))
	defer gh.Close()

	srv := newGithubTestServer(t, gh, "ghp_token")
	resp, err := http.Get(srv.URL + "/api/github/repos")
	if err != nil {
		t.Fatalf("GET /api/github/repos: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// githubRepoRowAt is githubRepoRow with a chosen updated_at, for pinning the
// merged order across installations.
func githubRepoRowAt(fullName, updatedAt string) map[string]any {
	row := githubRepoRow(fullName)
	row["updated_at"] = updatedAt
	return row
}
