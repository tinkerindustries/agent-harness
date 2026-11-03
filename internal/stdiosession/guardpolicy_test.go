package stdiosession

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/guard/guardtest"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

func bundleVersion(t *testing.T, edits ...func(map[string]any)) string {
	t.Helper()
	// The version is a hash over the bytes the harness receives, and a Go
	// client's encoder rewrites a raw message (it escapes angle brackets and
	// ampersands), so the expected version is of the encoded form.
	wire, err := json.Marshal(json.RawMessage(guardtest.BundleJSON(edits...)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := guard.LoadBundle(wire)
	if err != nil {
		t.Fatal(err)
	}
	return b.Version()
}

func withoutStage1(m map[string]any) { delete(m, "stage1") }
func otherCutoff(m map[string]any)   { m["cutoff"] = 0.4 }

func guardVersionOf(t *testing.T, frames []map[string]any) string {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("harness.guard frames = %d", len(frames))
	}
	g, _ := frames[0]["guard"].(map[string]any)
	v, _ := g["guard_version"].(string)
	return v
}

func TestGuardPolicyRefusals(t *testing.T) {
	fake := guardtest.New(t)
	secret := "Invented system prompt"
	cases := []struct {
		name   string
		mutate func(*CreateParams)
		code   int
		text   string
	}{
		{"no policy", func(p *CreateParams) { p.Harness.Guard.Policy = nil }, CodeInvalidParams, "harness.guard.policy"},
		{"invalid cutoff", func(p *CreateParams) {
			p.Harness.Guard.Policy = guardtest.BundleJSON(func(m map[string]any) { m["cutoff"] = 2 })
		}, CodeInvalidParams, "harness.guard.policy.cutoff"},
		{"empty stage 2 model", func(p *CreateParams) {
			p.Harness.Guard.Policy = guardtest.BundleJSON(func(m map[string]any) { m["stage2"].(map[string]any)["model"] = "" })
		}, CodeInvalidParams, "harness.guard.policy.stage2"},
		{"not a bundle", func(p *CreateParams) { p.Harness.Guard.Policy = []byte(`{"unrelated":true}`) }, CodeInvalidParams, "harness.guard.policy"},
		{"stage 1 and no endpoint", func(p *CreateParams) { p.Harness.Guard.Stage1Endpoint = "" }, CodeInvalidParams, "stage1_endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := guardFixture(t, fake, true, dsAnswer("never"))
			f.client.handshake(ClientCapabilities{})
			p := f.guardParams("run it", "shadow", fake)
			tc.mutate(&p)
			rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{})
			if rerr == nil || rerr.Code != tc.code || !strings.Contains(rerr.Message, tc.text) {
				t.Fatalf("create = %v, want %d naming %q", rerr, tc.code, tc.text)
			}
			if strings.Contains(rerr.Message, secret) {
				t.Fatalf("the refusal repeats bundle text: %q", rerr.Message)
			}
		})
	}
}

func TestGuardPolicyWithNoStageOneNeedsNoEndpoint(t *testing.T) {
	fake := guardtest.New(t)
	f, _ := guardFixture(t, fake, true,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`), dsAnswer("done"))
	f.client.handshake(ClientCapabilities{})
	p := f.guardParams("run it", "shadow", fake)
	p.Harness.Guard.Stage1Endpoint = ""
	p.Harness.Guard.Policy = guardtest.BundleJSON(withoutStage1)
	if rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	frames := guardFrames(f.client.waitFor(NotifyResponseCompleted))
	if got, want := guardVersionOf(t, frames), bundleVersion(t, withoutStage1); got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
	if len(fake.Stage1Requests()) != 0 || len(fake.Stage2Requests()) != 1 {
		t.Fatalf("stage 1 saw %d requests and stage 2 %d", len(fake.Stage1Requests()), len(fake.Stage2Requests()))
	}
}

func TestGuardOffWithAPolicyValidatesAndIgnoresIt(t *testing.T) {
	fake := guardtest.New(t)
	f, _ := guardFixture(t, fake, false, dsAnswer("fine"), dsAnswer("never"))
	f.client.handshake(ClientCapabilities{})

	bad := f.createParams("hello")
	bad.Model = testDeepSeekModel
	bad.Harness.Guard = &GuardConfig{Mode: "off", Policy: guardtest.BundleJSON(func(m map[string]any) { m["cutoff"] = 2 })}
	if rerr := f.client.call(MethodResponsesCreate, bad, &CreateResult{}); rerr == nil || !strings.Contains(rerr.Message, "harness.guard.policy.cutoff") {
		t.Fatalf("a malformed policy under off = %v", rerr)
	}

	ok := f.createParams("hello")
	ok.Model = testDeepSeekModel
	ok.Harness.Guard = &GuardConfig{Mode: "off", Policy: guardtest.BundleJSON()}
	if rerr := f.client.call(MethodResponsesCreate, ok, &CreateResult{}); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
	f.srv.mu.Lock()
	held := len(f.srv.bundles)
	f.srv.mu.Unlock()
	if held != 0 {
		t.Fatalf("an off session holds %d bundles", held)
	}
}

// The row holds the mode, the endpoint and the bundle's hash. Nothing of the
// bundle reaches the state directory.
func TestGuardRowStoresTheHashAndNoBundleText(t *testing.T) {
	fake := guardtest.New(t)
	dir, cwd := t.TempDir(), t.TempDir()
	f, _ := guardFixtureIn(t, dir, cwd, fake, true,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`), dsAnswer("done"))
	f.client.handshake(ClientCapabilities{})
	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", "shadow", fake), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)

	db, err := sql.Open("sqlite", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err := db.QueryRow(`SELECT guard FROM sessions WHERE id = ?`, created.Response.Harness.SessionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	want := `{"mode":"shadow","stage1_endpoint":"` + fake.Stage1URL + `","policy_hash":"` + bundleVersion(t) + `"}`
	if raw != want {
		t.Fatalf("guard column = %s, want %s", raw, want)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, marker := range []string{"Invented system prompt", "Invented instructions", guardtest.Question} {
			if strings.Contains(string(b), marker) {
				t.Errorf("%s holds bundle text %q", e.Name(), marker)
			}
		}
	}
}

// A process that holds the bundle keeps using it for a continuation that
// sends none, and a continuation that sends another replaces it.
func TestGuardContinuationUsesTheHeldBundleAndAReplacementChangesTheVersion(t *testing.T) {
	fake := guardtest.New(t)
	f, _ := guardFixture(t, fake, true,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`), dsAnswer("one"),
		dsToolCall("call-2", "bash", `{"command":"touch ran-2"}`), dsAnswer("two"),
		dsToolCall("call-3", "bash", `{"command":"touch ran-3"}`), dsAnswer("three"),
	)
	f.client.handshake(ClientCapabilities{})

	var one CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("first", "shadow", fake), &one); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	v1 := guardVersionOf(t, guardFrames(f.client.waitFor(NotifyResponseCompleted)))
	if v1 != bundleVersion(t) {
		t.Fatalf("version = %q, want the bundle's hash", v1)
	}

	next := resumeParams("", "second")
	next.Harness = nil
	next.PreviousResponseID = one.Response.ID
	var two CreateResult
	if rerr := f.client.call(MethodResponsesCreate, next, &two); rerr != nil {
		t.Fatalf("continuation: %v", rerr)
	}
	frames := guardFrames(f.client.waitFor(NotifyResponseCompleted))
	if got := guardVersionOf(t, frames[len(frames)-1:]); got != v1 {
		t.Fatalf("a continuation with no bundle recorded %q, want %q", got, v1)
	}

	third := resumeParams("", "third")
	third.Harness = &CreateHarness{Guard: &GuardConfig{Mode: "shadow", Policy: guardtest.BundleJSON(otherCutoff)}}
	third.PreviousResponseID = two.Response.ID
	if rerr := f.client.call(MethodResponsesCreate, third, &CreateResult{}); rerr != nil {
		t.Fatalf("replacement: %v", rerr)
	}
	frames = guardFrames(f.client.waitFor(NotifyResponseCompleted))
	v3 := guardVersionOf(t, frames[len(frames)-1:])
	if v3 != bundleVersion(t, otherCutoff) || v3 == v1 {
		t.Fatalf("replacement recorded %q", v3)
	}
	sess, err := f.srv.opts.Store.GetSession(t.Context(), one.Response.Harness.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Guard.PolicyHash != v3 {
		t.Fatalf("stored hash = %q, want %q", sess.Guard.PolicyHash, v3)
	}
}

// harness.close_session releases the bundle, so a resume of the stored row
// needs one again.
func TestCloseSessionReleasesTheBundle(t *testing.T) {
	fake := guardtest.New(t)
	f, _ := guardFixture(t, fake, true,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`), dsAnswer("one"), dsAnswer("never"))
	f.client.handshake(ClientCapabilities{})
	var one CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("first", "shadow", fake), &one); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
	id := one.Response.Harness.SessionID
	if f.srv.heldBundle(id) == nil {
		t.Fatal("the process holds no bundle for a guarded session")
	}
	if rerr := f.client.call(MethodCloseSession, CloseSessionParams{SessionID: id}, &map[string]any{}); rerr != nil {
		t.Fatalf("close: %v", rerr.Message)
	}
	if f.srv.heldBundle(id) != nil {
		t.Fatal("close_session left the bundle held")
	}
	if rerr := f.client.call(MethodResponsesCreate, resumeParams(id, "again"), &CreateResult{}); rerr == nil || !strings.Contains(rerr.Message, "harness.guard.policy") {
		t.Fatalf("a resume after close with no bundle = %v", rerr)
	}
}

// A compaction successor is screened with the bundle its parent held, and
// closing either id releases it.
func TestFollowCompactionCarriesTheBundleToTheSuccessor(t *testing.T) {
	b, err := guard.LoadBundle(guardtest.BundleJSON())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Options{})
	s.bundles["sess-original"] = b
	shells := tools.NewShells("sess-original", func(string, tools.ShellExit) {})
	defer shells.Close()

	if got := s.followCompaction("sess-original", &session.RunResult{SessionID: "sess-child"}, shells); got != "sess-child" {
		t.Fatalf("followCompaction = %q", got)
	}
	if s.heldBundle("sess-child") != b || s.heldBundle("sess-original") != b {
		t.Fatal("the successor does not hold the parent's bundle")
	}
	if _, rerr := s.closeSession(json.RawMessage(`{"session_id":"sess-original"}`)); rerr != nil {
		t.Fatalf("close: %v", rerr)
	}
	if s.heldBundle("sess-child") != nil || s.heldBundle("sess-original") != nil {
		t.Fatal("closing the original left a copy held")
	}
}
