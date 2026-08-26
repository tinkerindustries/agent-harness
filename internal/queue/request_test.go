package queue

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
	"github.com/mrgeoffrich/agent-harness/internal/skills"
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
		"deadline_ms": 60000,
		"job_type": "implementation",
		"parent_agent_type": "claude-code",
		"parent_agent_id": "sess-1",
		"parent_is_user": true
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
	if req.JobType != "implementation" || req.ParentAgentType != "claude-code" || req.ParentAgentID != "sess-1" || !req.ParentIsUser {
		t.Fatalf("unexpected provenance: %+v", req)
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
		{RequestID: "req-1", Prompt: "go", PermissionMode: "full"},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos()},
	}
	for _, req := range cases {
		if err := req.Validate(); err == nil {
			t.Fatalf("expected validation to fail for %+v", req)
		}
	}
}

// The prompt is optional: a browser start may create the run first and let
// the operator type the first message into the session, so an empty prompt
// is valid as long as everything the run actually needs is present.
func TestValidateAcceptsEmptyPrompt(t *testing.T) {
	req := Request{RequestID: "req-1", Repos: testRepos(), PermissionMode: "full"}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A named model must be in the model→provider table: an unknown model fails
// loudly at validation instead of silently defaulting to a provider
// (internal/provider, docs/KIMI-INTEGRATION.md §4.3). Empty means "the
// default model" and stays valid.
func TestValidateChecksModelAgainstProviderTable(t *testing.T) {
	for _, model := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "kimi-k3", "gemini-3.7-flash"} {
		req := Request{RequestID: "req-1", Repos: testRepos(), PermissionMode: "full", Model: model}
		if err := req.Validate(); err != nil {
			t.Errorf("Validate with model %q: %v", model, err)
		}
	}
	for _, model := range []string{"deepseek-v4-turbo", "kimi-k2.6", "gpt-4"} {
		req := Request{RequestID: "req-1", Repos: testRepos(), PermissionMode: "full", Model: model}
		if err := req.Validate(); err == nil {
			t.Errorf("Validate with unknown model %q: nil error, want one", model)
		}
	}
	req := Request{RequestID: "req-1", Repos: testRepos(), PermissionMode: "full"}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate with empty model (default): %v", err)
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

// The provenance fields are optional: a request that knows nothing about
// them, and one that names a valid job type and parent agent, both pass.
// parent_is_user is producer-stamped, so a user-started request carries it
// and never a parent agent type.
func TestValidateAcceptsProvenanceFields(t *testing.T) {
	cases := []Request{
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
			JobType: agentmeta.JobTypeOrchestration, ParentAgentType: "orchestrator", ParentAgentID: "orch-1"},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
			JobType: agentmeta.JobTypeImplementation, ParentIsUser: true},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
			ParentIsUser: true, ParentAgentID: "geoff"},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full"},
	}
	for _, req := range cases {
		if err := req.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
	}
}

// A present-but-malformed provenance field fails validation; an absent one
// never does. The retired type "user" is rejected even without an id, and a
// user-started run must not carry a parent agent type.
func TestValidateRejectsMalformedProvenanceFields(t *testing.T) {
	cases := []Request{
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full", JobType: "orchestrator"},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full", ParentAgentID: "id-with-no-type"},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
			ParentAgentType: agentmeta.ParentAgentUser},
		{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
			ParentIsUser: true, ParentAgentType: "claude-code"},
	}
	for _, req := range cases {
		if err := req.Validate(); err == nil {
			t.Errorf("expected validation to fail for %+v", req)
		}
	}
}

// words returns n whitespace-separated words, for building a title or
// description exactly at a word cap or one word over.
func words(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "word"
	}
	return strings.Join(parts, " ")
}

// TestValidateTitleAndDescriptionCaps pins the word caps on the run's title
// and description: ten words pass and eleven fail for the title, fifty pass
// and fifty-one fail for the description, and empty stays valid — presence
// is the producer's call at this layer (a browser start may leave both
// blank, the same reasoning the prompt is optional).
func TestValidateTitleAndDescriptionCaps(t *testing.T) {
	base := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full"}

	base.Title = ""
	base.Description = ""
	if err := base.Validate(); err != nil {
		t.Fatalf("empty title and description must pass validation: %v", err)
	}

	base.Title = words(agentmeta.MaxTitleWords)
	if err := base.Validate(); err != nil {
		t.Fatalf("a %d-word title must pass: %v", agentmeta.MaxTitleWords, err)
	}
	base.Title = words(agentmeta.MaxTitleWords + 1)
	if err := base.Validate(); err == nil {
		t.Fatal("an 11-word title must fail validation")
	}

	base.Title = ""
	base.Description = words(agentmeta.MaxDescriptionWords)
	if err := base.Validate(); err != nil {
		t.Fatalf("a %d-word description must pass: %v", agentmeta.MaxDescriptionWords, err)
	}
	base.Description = words(agentmeta.MaxDescriptionWords + 1)
	if err := base.Validate(); err == nil {
		t.Fatal("a 51-word description must fail validation")
	}
}

// TestValidatePhaseRelationship pins the phase pair on the wire: both zero
// (unphased) passes, a set pair must satisfy phase <= total_phases, and the
// failure is wrapped with the queue: prefix like every other agentmeta
// rejection so a caller can see which layer refused the request.
func TestValidatePhaseRelationship(t *testing.T) {
	base := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full"}

	for _, tc := range []struct {
		phase, total int
		ok           bool
	}{
		{0, 0, true},
		{1, 3, true},
		{3, 3, true},
		{2, 1, false},
		{0, 3, false},
		{2, 0, false},
		{-1, 3, false},
	} {
		req := base
		req.Phase, req.TotalPhases = tc.phase, tc.total
		err := req.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("phase %d/%d: error = %v, want error = %v", tc.phase, tc.total, err, !tc.ok)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "queue: ") {
			t.Errorf("phase %d/%d: error %q must be wrapped with the queue: prefix", tc.phase, tc.total, err)
		}
	}
}

// TestValidateAttachmentIDs pins the attachment_ids contract: valid ids
// pass, and an empty id, whitespace, or a duplicate is refused with a
// message naming the offending index. The count and byte caps live where
// attachments are accepted, not here.
func TestValidateAttachmentIDs(t *testing.T) {
	base := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full"}

	base.AttachmentIDs = []string{"att-1111111111111111", "att-2222222222222222"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid attachment ids refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{name: "empty id", ids: []string{""}, want: "attachment_ids[0] is empty"},
		{name: "whitespace", ids: []string{"att-1111 2222"}, want: "contains whitespace"},
		{name: "duplicate", ids: []string{"att-1111111111111111", "att-1111111111111111"}, want: "is duplicated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base.AttachmentIDs = tc.ids
			err := base.Validate()
			if err == nil {
				t.Fatal("expected validation to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}

	// The ids ride the wire: a published request carries them, so a producer
	// that wrote the rows can point the worker at them.
	body, err := json.Marshal(Request{RequestID: "req-1", Repos: testRepos(), PermissionMode: "full", AttachmentIDs: []string{"att-1111111111111111"}})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.AttachmentIDs) != 1 || parsed.AttachmentIDs[0] != "att-1111111111111111" {
		t.Errorf("attachment_ids did not round-trip: %+v", parsed.AttachmentIDs)
	}
}

// A resume names a session instead of repositories: the workspace it
// continues in already exists, with the clones the original request made
// still in it (docs/RUN-CONTROL.md "Continuing").
func TestValidateAcceptsResumeWithoutRepos(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "keep going", PermissionMode: "full", ResumeSessionID: "sess-1"}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate on a resume request: %v", err)
	}
}

// Refused rather than ignored: a producer that set both fields meant to
// start a run and would otherwise get a resume with its repositories
// silently dropped.
func TestValidateRejectsResumeCarryingRepos(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "keep going", PermissionMode: "full", ResumeSessionID: "sess-1", Repos: testRepos()}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected a resume request carrying repos to be rejected")
	}
	if !strings.Contains(err.Error(), "repos must be empty") {
		t.Fatalf("error = %v, want it to name the repos rule", err)
	}
}

// Relaxing the repos rule relaxes nothing else: the resume route copies the
// session's own permission mode and model onto the request precisely so
// every other check still applies to it unchanged.
func TestValidateStillChecksTheRestOfAResumeRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"no permission mode", Request{RequestID: "req-1", Prompt: "go", ResumeSessionID: "sess-1"}},
		{"bad permission mode", Request{RequestID: "req-1", Prompt: "go", ResumeSessionID: "sess-1", PermissionMode: "sudo"}},
		{"unknown model", Request{RequestID: "req-1", Prompt: "go", ResumeSessionID: "sess-1", PermissionMode: "full", Model: "no-such-model"}},
		{"no request id", Request{Prompt: "go", ResumeSessionID: "sess-1", PermissionMode: "full"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.req.Validate(); err == nil {
				t.Fatalf("expected %s to be rejected on a resume request too", tc.name)
			}
		})
	}
}

// A pack a run can actually be given is accepted, and the default — no packs
// at all — stays valid, because that is what every producer sends unless
// somebody asked for one (internal/skills, "Packs").
func TestValidateAcceptsKnownSkillPacks(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
		SkillPacks: []string{skills.PackUnity}}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	req.SkillPacks = nil
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate with no packs: %v", err)
	}
}

// An unknown pack is refused rather than dropped. A silently ignored typo
// produces a run that lacks the skills somebody asked for, and the symptom —
// the model not knowing something — points nowhere near the misspelt
// argument that caused it.
func TestValidateRejectsUnknownSkillPack(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "go", Repos: testRepos(), PermissionMode: "full",
		SkillPacks: []string{"unitie"}}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected an unknown skill pack to be rejected")
	}
	if !strings.Contains(err.Error(), "unitie") {
		t.Fatalf("error should name the offending pack, got %v", err)
	}
}

// A resume builds no workspace, so there is nothing for a pack to be
// installed into and the packs the original request asked for are still
// there. Refusing rather than ignoring is what stops a producer believing it
// added skills to a session mid-flight.
func TestValidateRejectsSkillPacksOnResume(t *testing.T) {
	req := Request{RequestID: "req-1", Prompt: "carry on", PermissionMode: "full",
		ResumeSessionID: "sess-1", SkillPacks: []string{skills.PackUnity}}
	if err := req.Validate(); err == nil {
		t.Fatal("expected skill_packs to be refused on a resume request")
	}
}
