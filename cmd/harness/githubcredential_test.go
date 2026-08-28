package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/githubauth"
)

// TestReadCredentialRequestParsesGitsProtocol pins the shape git speaks: one
// key=value per line, terminated by a blank line, and a value that itself
// contains "=" kept whole.
func TestReadCredentialRequestParsesGitsProtocol(t *testing.T) {
	in := strings.NewReader("protocol=https\nhost=github.com\npath=SomeOrg/app.git\nwwwauth[]=Basic realm=\"GitHub\"\n\ntrailing=ignored\n")
	got, err := readCredentialRequest(in)
	if err != nil {
		t.Fatalf("readCredentialRequest: %v", err)
	}
	if got["host"] != "github.com" || got["path"] != "SomeOrg/app.git" {
		t.Errorf("parsed %v, want the host and path git sent", got)
	}
	if got["wwwauth[]"] != `Basic realm="GitHub"` {
		t.Errorf("wwwauth[] = %q, want the whole value after the first =", got["wwwauth[]"])
	}
	if _, ok := got["trailing"]; ok {
		t.Error("read past the blank line that ends git's request")
	}
}

func TestCredentialOwnerTakesTheFirstPathSegment(t *testing.T) {
	for path, want := range map[string]string{
		"SomeOrg/app.git":       "SomeOrg",
		"/SomeOrg/app":          "SomeOrg",
		"mrgeoffrich/notes.git": "mrgeoffrich",
		"":                      "",
	} {
		if got := credentialOwner(map[string]string{"path": path}); got != want {
			t.Errorf("credentialOwner(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestRunGitHubCredentialAnswersAGet drives the helper end to end against a
// stub harness: git's request on stdin, the credential on stdout in git's
// own shape.
func TestRunGitHubCredentialAnswersAGet(t *testing.T) {
	var gotOwner string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cred-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid bearer token"}`))
			return
		}
		var body struct{ Owner string }
		json.NewDecoder(r.Body).Decode(&body)
		gotOwner = body.Owner
		json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "password": "ghs_org"})
	}))
	defer srv.Close()

	out := withHelperEnv(t, srv.URL, "cred-token", "protocol=https\nhost=github.com\npath=SomeOrg/app.git\n\n", "get")
	if gotOwner != "SomeOrg" {
		t.Errorf("asked the harness for %q, want SomeOrg", gotOwner)
	}
	if !strings.Contains(out, "username=x-access-token\n") || !strings.Contains(out, "password=ghs_org\n") {
		t.Errorf("helper wrote %q, want git's username and password lines", out)
	}
}

// TestRunGitHubCredentialStaysSilentWhereItHasNothingToSay pins the three
// cases where the helper answers with nothing and exit 0, which git reads as
// "no credential from this helper" rather than as a failure: another host, a
// store or erase operation, and a harness that published no callback (the
// personal-access-token path).
func TestRunGitHubCredentialStaysSilentWhereItHasNothingToSay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the helper called the harness when it should have stayed silent")
	}))
	defer srv.Close()

	t.Run("another host", func(t *testing.T) {
		if out := withHelperEnv(t, srv.URL, "cred-token", "protocol=https\nhost=gitlab.com\npath=someone/thing.git\n\n", "get"); out != "" {
			t.Errorf("helper wrote %q, want nothing", out)
		}
	})
	t.Run("store", func(t *testing.T) {
		if out := withHelperEnv(t, srv.URL, "cred-token", "protocol=https\nhost=github.com\npath=SomeOrg/app.git\n\n", "store"); out != "" {
			t.Errorf("helper wrote %q, want nothing", out)
		}
	})
	t.Run("no callback published", func(t *testing.T) {
		if out := withHelperEnv(t, "", "", "protocol=https\nhost=github.com\npath=SomeOrg/app.git\n\n", "get"); out != "" {
			t.Errorf("helper wrote %q, want nothing", out)
		}
	})
}

// withHelperEnv runs the helper with stdin and stdout redirected and the
// callback environment set, returning what it wrote to stdout.
func withHelperEnv(t *testing.T, endpoint, token, stdin string, args ...string) string {
	t.Helper()
	if endpoint == "" {
		os.Unsetenv(githubauth.EnvCredentialURL)
	} else {
		t.Setenv(githubauth.EnvCredentialURL, endpoint+"/api/github/credential")
	}
	if token == "" {
		os.Unsetenv(githubauth.EnvCredentialToken)
	} else {
		t.Setenv(githubauth.EnvCredentialToken, token)
	}

	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inRead, outWrite
	t.Cleanup(func() { os.Stdin, os.Stdout = oldIn, oldOut })

	go func() {
		io.WriteString(inWrite, stdin)
		inWrite.Close()
	}()
	runErr := runGitHubCredential(context.Background(), args)
	outWrite.Close()
	written, _ := io.ReadAll(outRead)
	if runErr != nil {
		t.Fatalf("runGitHubCredential: %v", runErr)
	}
	return string(written)
}

func TestShellQuoteSurvivesAwkwardPaths(t *testing.T) {
	if got := shellQuote("/opt/my harness/bin/harness"); got != "'/opt/my harness/bin/harness'" {
		t.Errorf("shellQuote = %q", got)
	}
	if got := shellQuote("/opt/it's/harness"); got != `'/opt/it'\''s/harness'` {
		t.Errorf("shellQuote = %q", got)
	}
}

func TestLoopbackBaseURLKeepsOnlyThePort(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"0.0.0.0:8180":   "http://127.0.0.1:8180",
		":8080":          "http://127.0.0.1:8080",
		"nonsense":       "",
	} {
		if got := loopbackBaseURL(addr); got != want {
			t.Errorf("loopbackBaseURL(%q) = %q, want %q", addr, got, want)
		}
	}
}
