package stdiosession

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/guard/guardtest"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// guardFixture is a Responses-vocabulary server whose guard talks to fake
// stage 1 and stage 2 servers. hasKey is what the server reports for
// ANTHROPIC_API_KEY.
func guardFixture(t *testing.T, fake *guardtest.Fake, hasKey bool, streams ...string) (*fixture, *deepseekRecorder) {
	t.Helper()
	return guardFixtureIn(t, t.TempDir(), t.TempDir(), fake, hasKey, streams...)
}

func guardFixtureIn(t *testing.T, dir, cwd string, fake *guardtest.Fake, hasKey bool, streams ...string) (*fixture, *deepseekRecorder) {
	t.Helper()
	return newDeepSeekFixtureIn(t, dir, cwd, func(r *session.Runner, o *Options) {
		r.Guards = func(setting store.GuardSetting) (session.GuardDecider, error) {
			return guard.NewPipeline(guard.PipelineConfig{
				Stage1Endpoint: setting.Stage1Endpoint,
				Stage2Model:    setting.Stage2Model,
				AnthropicKey:   "sk-ant-test",
				Stage2BaseURL:  fake.Stage2URL,
			})
		}
		o.Guard = &GuardOptions{HasAPIKey: func() bool { return hasKey }}
	}, streams...)
}

func (f *fixture) guardParams(text, mode string, fake *guardtest.Fake) CreateParams {
	p := f.createParams(text)
	p.Model = testDeepSeekModel
	p.Harness.PermissionMode = "full"
	p.Harness.Guard = &GuardConfig{Mode: mode, Stage1Endpoint: fake.Stage1URL}
	return p
}

func guardFrames(ms []message) []map[string]any {
	var out []map[string]any
	for _, m := range ms {
		if m.Method != NotifyGuard {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal(m.Params, &frame); err == nil {
			out = append(out, frame)
		}
	}
	return out
}

func TestGuardShadowRunsTheCommandAndSendsAGuardFrame(t *testing.T) {
	fake := guardtest.New(t)
	f, _ := guardFixture(t, fake, true,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
		dsAnswer("All done."),
	)
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", "shadow", fake), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	if _, err := os.Stat(filepath.Join(f.cwd, "ran-1")); err != nil {
		t.Fatal("the command did not run")
	}
	frames := guardFrames(seen)
	if len(frames) != 1 {
		t.Fatalf("harness.guard frames = %d; saw %v", len(frames), methodsOf(seen))
	}
	g, _ := frames[0]["guard"].(map[string]any)
	if frames[0]["call_id"] != "call-1" || g["mode"] != "shadow" || g["verdict"] != "allow" || g["decided_by"] != "stage2" || g["guard_version"] == "" {
		t.Fatalf("frame = %v", frames[0])
	}
	// The frame comes before the run's terminal frame.
	if idx := indexOfMethod(seen, NotifyGuard); idx < 0 || idx > indexOfMethod(seen, NotifyResponseCompleted) {
		t.Fatalf("order = %v", methodsOf(seen))
	}
	if len(fake.Stage2Requests()) != 1 {
		t.Fatalf("stage 2 requests = %d", len(fake.Stage2Requests()))
	}
}

func indexOfMethod(ms []message, method string) int {
	for i, m := range ms {
		if m.Method == method {
			return i
		}
	}
	return -1
}

func TestGuardShadowStageTwoOutageStillRunsTheCommand(t *testing.T) {
	fake := guardtest.New(t)
	fake.Set(func(b *guardtest.Behaviour) { b.Stage2Status = http.StatusServiceUnavailable })
	f, _ := guardFixture(t, fake, true,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
		dsAnswer("All done."),
	)
	f.client.handshake(ClientCapabilities{})

	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", "shadow", fake), &CreateResult{}); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)
	if _, err := os.Stat(filepath.Join(f.cwd, "ran-1")); err != nil {
		t.Fatal("a stage 2 outage stopped the command")
	}
	frames := guardFrames(seen)
	if len(frames) != 1 {
		t.Fatalf("harness.guard frames = %d", len(frames))
	}
	g, _ := frames[0]["guard"].(map[string]any)
	s2, _ := g["stage2"].(map[string]any)
	if g["verdict"] != "block" || s2["error"] == nil || s2["error"] == "" {
		t.Fatalf("frame = %v", frames[0])
	}
}

func TestGuardCreateRefusals(t *testing.T) {
	fake := guardtest.New(t)
	cases := []struct {
		name   string
		hasKey bool
		mutate func(*CreateParams)
		code   int
		text   string
	}{
		{"enforce", true, func(p *CreateParams) { p.Harness.Guard.Mode = "enforce" }, CodeUnsupported, "enforce"},
		{"no anthropic key", false, func(*CreateParams) {}, CodeCredentialsMissing, "ANTHROPIC_API_KEY"},
		{"readonly session", true, func(p *CreateParams) { p.Harness.PermissionMode = "readonly" }, CodeInvalidParams, "readonly"},
		{"missing mode", true, func(p *CreateParams) { p.Harness.Guard.Mode = "" }, CodeInvalidParams, "mode"},
		{"unknown mode", true, func(p *CreateParams) { p.Harness.Guard.Mode = "audit" }, CodeInvalidParams, "mode"},
		{"no endpoint", true, func(p *CreateParams) { p.Harness.Guard.Stage1Endpoint = "" }, CodeInvalidParams, "stage1_endpoint"},
		{"bad endpoint", true, func(p *CreateParams) { p.Harness.Guard.Stage1Endpoint = "ollama.local:11434" }, CodeInvalidParams, "stage1_endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := guardFixture(t, fake, tc.hasKey, dsAnswer("never"))
			f.client.handshake(ClientCapabilities{})
			p := f.guardParams("run it", "shadow", fake)
			tc.mutate(&p)
			rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{})
			if rerr == nil || rerr.Code != tc.code || !strings.Contains(rerr.Message, tc.text) {
				t.Fatalf("create = %v, want %d naming %q", rerr, tc.code, tc.text)
			}
		})
	}
}

func TestGuardOffNeedsNoKeyAndAReadonlySessionMayNameIt(t *testing.T) {
	fake := guardtest.New(t)
	f, _ := guardFixture(t, fake, false, dsAnswer("fine"))
	f.client.handshake(ClientCapabilities{})
	p := f.createParams("hello")
	p.Model = testDeepSeekModel
	p.Harness.Guard = &GuardConfig{Mode: "off"}
	if rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
}

func TestOtherSubcommandsRefuseTheGuardSetting(t *testing.T) {
	fake := guardtest.New(t)

	t.Run("gemini-session", func(t *testing.T) {
		useDialect(t, NewInteractions())
		f := newFixture(t, answer("never"))
		f.client.handshake(ClientCapabilities{})
		p := f.createParams("hello")
		p.Harness.PermissionMode = "full"
		p.Harness.Guard = &GuardConfig{Mode: "shadow", Stage1Endpoint: fake.Stage1URL}
		rerr := f.client.call(MethodInteractionsCreate, p, &iactCreateResult{})
		if rerr == nil || rerr.Code != CodeUnsupported || !strings.Contains(rerr.Message, "harness.guard") {
			t.Fatalf("interactions.create = %v", rerr)
		}
	})

	t.Run("claude-session", func(t *testing.T) {
		f, _ := newManagedAgentsFixture(t, 0, "")
		f.client.handshake(ClientCapabilities{})
		p := maCreateParamsFor(f, "hello", nil)
		p.Harness.PermissionMode = "full"
		p.Harness.Guard = &GuardConfig{Mode: "shadow", Stage1Endpoint: fake.Stage1URL}
		rerr := f.client.call(MethodSessionsCreate, p, &json.RawMessage{})
		if rerr == nil || rerr.Code != CodeUnsupported || !strings.Contains(rerr.Message, "harness.guard") {
			t.Fatalf("sessions.create = %v", rerr)
		}
	})
}

// A resumed session takes the mode its create names, and the stored one when
// the create names none.
func TestGuardResumeTakesTheCreatesMode(t *testing.T) {
	fake := guardtest.New(t)
	dir, cwd := t.TempDir(), t.TempDir()

	a, _ := guardFixtureIn(t, dir, cwd, fake, true,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`), dsAnswer("one"))
	a.client.handshake(ClientCapabilities{})
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, a.guardParams("first", "shadow", fake), &one); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	if n := len(guardFrames(a.client.waitFor(NotifyResponseCompleted))); n != 1 {
		t.Fatalf("first run guard frames = %d", n)
	}
	sessionID := one.Response.Harness.SessionID
	if rerr := a.client.call(MethodShutdown, map[string]any{}, nil); rerr != nil {
		t.Fatal(rerr)
	}
	a.close()

	// Second process, the create names no guard: the stored shadow applies.
	b, _ := guardFixtureIn(t, dir, cwd, fake, true,
		dsToolCall("call-2", "bash", `{"command":"touch ran-2"}`), dsAnswer("two"))
	b.client.handshake(ClientCapabilities{})
	if rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second"), &CreateResult{}); rerr != nil {
		t.Fatalf("resume: %v", rerr)
	}
	if n := len(guardFrames(b.client.waitFor(NotifyResponseCompleted))); n != 1 {
		t.Fatalf("a resume naming no guard: frames = %d, want the stored shadow's 1", n)
	}
	if err := b.client.call(MethodShutdown, map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	b.close()

	// Third process, the create turns the guard off: it is off and stays off.
	c, _ := guardFixtureIn(t, dir, cwd, fake, true,
		dsToolCall("call-3", "bash", `{"command":"touch ran-3"}`), dsAnswer("three"))
	c.client.handshake(ClientCapabilities{})
	p := resumeParams(sessionID, "third")
	p.Harness.Guard = &GuardConfig{Mode: "off"}
	if rerr := c.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr != nil {
		t.Fatalf("resume off: %v", rerr)
	}
	if n := len(guardFrames(c.client.waitFor(NotifyResponseCompleted))); n != 0 {
		t.Fatalf("a resume naming off: frames = %d", n)
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran-3")); err != nil {
		t.Fatal("the command did not run")
	}
}

// A resume that names the guard on a session started without one is screened
// from then on.
func TestGuardCanBeTurnedOnByAResume(t *testing.T) {
	fake := guardtest.New(t)
	dir, cwd := t.TempDir(), t.TempDir()

	a, _ := guardFixtureIn(t, dir, cwd, fake, true, dsAnswer("one"))
	a.client.handshake(ClientCapabilities{})
	p := a.createParams("first")
	p.Model = testDeepSeekModel
	p.Harness.PermissionMode = "full"
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, p, &one); rerr != nil {
		t.Fatal(rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	_ = a.client.call(MethodShutdown, map[string]any{}, nil)
	a.close()

	b, _ := guardFixtureIn(t, dir, cwd, fake, true,
		dsToolCall("call-2", "bash", `{"command":"touch ran-2"}`), dsAnswer("two"))
	b.client.handshake(ClientCapabilities{})
	rp := resumeParams(sessionID, "second")
	rp.Harness.Guard = &GuardConfig{Mode: "shadow", Stage1Endpoint: fake.Stage1URL}
	if rerr := b.client.call(MethodResponsesCreate, rp, &CreateResult{}); rerr != nil {
		t.Fatalf("resume: %v", rerr)
	}
	if n := len(guardFrames(b.client.waitFor(NotifyResponseCompleted))); n != 1 {
		t.Fatalf("frames = %d, want 1", n)
	}
}
