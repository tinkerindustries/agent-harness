package queue

import (
	"encoding/json"
	"testing"
)

func TestParseRequestRoundTrips(t *testing.T) {
	body := `{
		"request_id": "req-1",
		"prompt": "do the thing",
		"repos": [{"url": "https://example.com/org/app.git"}, {"url": "https://example.com/org/lib.git", "branch": "next"}],
		"model": "deepseek-v4-flash",
		"effort": "low",
		"permission_mode": "default",
		"deny": ["git push"],
		"result_schema": {"type": "object"},
		"max_sub_turns": 10,
		"deadline_ms": 60000
	}`
	req, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.RequestID != "req-1" || req.Prompt != "do the thing" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if len(req.Repos) != 2 || req.Repos[0].URL != "https://example.com/org/app.git" || req.Repos[1].Branch != "next" {
		t.Fatalf("unexpected repos: %+v", req.Repos)
	}
	if req.Model != "deepseek-v4-flash" || req.Effort != "low" || req.PermissionMode != "default" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if len(req.Deny) != 1 || req.Deny[0] != "git push" {
		t.Fatalf("unexpected deny: %+v", req.Deny)
	}
	if req.MaxSubTurns != 10 || req.DeadlineMS != 60000 {
		t.Fatalf("unexpected limits: %+v", req)
	}
}

func TestParseRequestRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseRequest([]byte(`{not json`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// testRepos is one valid repository, the minimum a request needs to pass
// validation.
func testRepos() []Repo {
	return []Repo{{URL: "https://example.com/org/app.git"}}
}

func TestBranchOrDefault(t *testing.T) {
	if got := (Repo{URL: "https://example.com/org/app.git"}).BranchOrDefault(); got != "main" {
		t.Fatalf("expected main, got %q", got)
	}
	if got := (Repo{URL: "https://example.com/org/app.git", Branch: "next"}).BranchOrDefault(); got != "next" {
		t.Fatalf("expected next, got %q", got)
	}
}

func TestRepoDir(t *testing.T) {
	cases := map[string]string{
		"https://example.com/org/app.git":   "app",
		"https://example.com/org/app":       "app",
		"https://example.com/org/app/":      "app",
		"git@example.com:org/app.git":       "app",
		"ssh://git@example.com/org/app.git": "app",
	}
	for url, want := range cases {
		if got := (Repo{URL: url}).Dir(); got != want {
			t.Errorf("Dir(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestValidateAcceptsRepos(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full"}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	cases := []Request{
		{Prompt: "go", Repos: testRepos(), PermissionMode: "full"},
		{RequestID: "req-1", Repos: testRepos(), PermissionMode: "full"},
		{RequestID: "req-1", Prompt: "go", PermissionMode: "full"},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos()},
	}
	for _, req := range cases {
		if err := req.Validate(); err == nil {
			t.Fatalf("expected validation to fail for %+v", req)
		}
	}
}

func TestValidateRejectsRequestIDWithSubjectMetacharacters(t *testing.T) {
	req := Request{RequestID: "req.1", Prompt: "go", Repos: testRepos(), PermissionMode: "full"}
	if err := req.Validate(); err == nil {
		t.Fatal("expected a request_id containing '.' to be rejected")
	}
}

// A repository URL reaches git clone, and git treats some transports as a
// command to run rather than a place to fetch from.
func TestValidateRejectsUnsupportedRepoURLs(t *testing.T) {
	cases := []string{
		"",
		"ext::sh -c 'touch /tmp/pwned'",
		"file:///etc",
		"/etc/passwd",
		"--upload-pack=touch /tmp/pwned",
		"https://",
	}
	for _, url := range cases {
		req := Request{RequestID: "req-1", Prompt: "go", Repos: []Repo{{URL: url}}, PermissionMode: "full"}
		if err := req.Validate(); err == nil {
			t.Errorf("expected repo url %q to be rejected", url)
		}
	}
}

func TestValidateAcceptsSupportedRepoURLs(t *testing.T) {
	cases := []string{
		"https://example.com/org/app.git",
		"http://example.com/org/app.git",
		"ssh://git@example.com/org/app.git",
		"git://example.com/org/app.git",
		"git@example.com:org/app.git",
	}
	for _, url := range cases {
		req := Request{RequestID: "req-1", Prompt: "go", Repos: []Repo{{URL: url}}, PermissionMode: "full"}
		if err := req.Validate(); err != nil {
			t.Errorf("repo url %q: %v", url, err)
		}
	}
}

func TestValidateRejectsUnusableBranchNames(t *testing.T) {
	cases := []string{"-b", "feature branch", "a..b", "re:f", "ref^", "ref~1", "ref?"}
	for _, branch := range cases {
		req := Request{RequestID: "req-1", Prompt: "go", PermissionMode: "full",
			Repos: []Repo{{URL: "https://example.com/org/app.git", Branch: branch}}}
		if err := req.Validate(); err == nil {
			t.Errorf("expected branch %q to be rejected", branch)
		}
	}
}

// Two repositories whose URLs end in the same name would clone into one
// directory, the second one failing on a path that already exists.
func TestValidateRejectsCollidingRepoNames(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "go", PermissionMode: "full", Repos: []Repo{
		{URL: "https://example.com/one/app.git"},
		{URL: "https://example.com/two/app.git"},
	}}
	if err := req.Validate(); err == nil {
		t.Fatal("expected two repositories cloning into the same directory to be rejected")
	}
}

func TestValidateRequiresAtLeastOneRepo(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "go", PermissionMode: "full"}
	if err := req.Validate(); err == nil {
		t.Fatal("expected a request with no repos to be rejected")
	}
}

func TestValidateRejectsInvalidPermissionMode(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "sudo"}
	if err := req.Validate(); err == nil {
		t.Fatal("expected an invalid permission_mode to be rejected")
	}
}

func TestValidateAcceptsEveryPermissionMode(t *testing.T) {
	for _, mode := range []string{"readonly", "full"} {
		req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: mode}
		if err := req.Validate(); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
}

// permission_mode is required, and "default" is no longer one of its
// values: a queue client still sending the removed mode fails loudly
// rather than running under a substituted one.
func TestValidateRequiresPermissionModeAndRejectsRemovedDefault(t *testing.T) {
	for _, mode := range []string{"", "default"} {
		req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: mode}
		if err := req.Validate(); err == nil {
			t.Errorf("expected permission_mode %q to be rejected", mode)
		}
	}
}

func TestValidateRejectsMalformedResultSchema(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`not json`),
		json.RawMessage(`["not", "an", "object"]`),
		json.RawMessage(`"a string"`),
	}
	for _, schema := range cases {
		req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), ResultSchema: schema, PermissionMode: "full"}
		if err := req.Validate(); err == nil {
			t.Fatalf("expected result_schema %s to be rejected", schema)
		}
	}
}

func TestValidateAcceptsWellFormedResultSchema(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
		ResultSchema: json.RawMessage(`{"type":"object","properties":{}}`)}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsNegativeLimits(t *testing.T) {
	if err := (Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full", MaxSubTurns: -1}).Validate(); err == nil {
		t.Fatal("expected negative max_sub_turns to be rejected")
	}
	if err := (Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full", DeadlineMS: -1}).Validate(); err == nil {
		t.Fatal("expected negative deadline_ms to be rejected")
	}
}
