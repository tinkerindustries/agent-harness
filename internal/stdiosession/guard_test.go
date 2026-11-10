package stdiosession

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/providerhttp"
	"github.com/mrgeoffrich/agent-harness/internal/session"
)

// fakeClassifier is a `POST /classify` endpoint that answers with a fixed
// status and body and records every request it was sent.
type fakeClassifier struct {
	mu       sync.Mutex
	status   int
	body     string
	requests []map[string]any
	srv      *httptest.Server
}

func newFakeClassifier(t *testing.T, status int, body string) *fakeClassifier {
	t.Helper()
	c := &fakeClassifier{status: status, body: body}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.mu.Lock()
		c.requests = append(c.requests, req)
		status, body := c.status, c.body
		c.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeClassifier) seen() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.requests...)
}

const classifyBlock = `{"verdict":"block","reason":"the command deletes the repository","decided_by":"stage2","guard_version":"v9"}`
const classifyAllow = `{"verdict":"allow","reason":"","decided_by":"rules","guard_version":"v9"}`

// approvalClient claims the approvals capability and answers every
// harness.approval_request with answer, recording what it was asked.
type approvalClient struct {
	mu    sync.Mutex
	asked []ApprovalRequestParams
}

func (a *approvalClient) install(f *fixture, answer func(ApprovalRequestParams) (any, *rpcError)) {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	f.client.onRequest = func(m message) (any, *rpcError) {
		if m.Method != MethodApprovalRequest {
			return nil, errorf(CodeMethodNotFound, "no %s", m.Method)
		}
		var p ApprovalRequestParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, errorf(CodeInvalidParams, "%v", err)
		}
		a.mu.Lock()
		a.asked = append(a.asked, p)
		a.mu.Unlock()
		return answer(p)
	}
}

func (a *approvalClient) requests() []ApprovalRequestParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ApprovalRequestParams(nil), a.asked...)
}

func guardFixture(t *testing.T, streams ...string) (*fixture, *deepseekRecorder) {
	t.Helper()
	return newDeepSeekFixtureIn(t, t.TempDir(), t.TempDir(), func(_ *session.Runner, o *Options) {
		o.Guard = &GuardOptions{}
	}, streams...)
}

func (f *fixture) guardParams(text, classifyURL string) CreateParams {
	p := f.createParams(text)
	p.Model = testDeepSeekModel
	p.Harness.PermissionMode = "full"
	p.Harness.Guard = &GuardConfig{ClassifyURL: classifyURL, Scenario: "repo", Engine: "deepseek"}
	return p
}

func ran(f *fixture, name string) bool {
	_, err := os.Stat(filepath.Join(f.cwd, name))
	return err == nil
}

// An allow verdict runs the command and never asks the parent. The classifier
// is sent the whole contract: the command, the shell, the directory the
// command runs in, the scenario and engine the create named, and the ids.
func TestGuardAllowRunsTheCommandWithoutAsking(t *testing.T) {
	cls := newFakeClassifier(t, 200, classifyAllow)
	f, _ := guardFixture(t,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
		dsAnswer("All done."),
	)
	f.client.handshake(ClientCapabilities{Approvals: true})
	var approvals approvalClient
	approvals.install(f, func(ApprovalRequestParams) (any, *rpcError) {
		return ApprovalAnswerResult{Decision: "deny"}, nil
	})

	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", cls.srv.URL+"/classify"), &CreateResult{}); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)

	if !ran(f, "ran-1") {
		t.Fatal("an allowed command did not run")
	}
	if n := len(approvals.requests()); n != 0 {
		t.Fatalf("the parent was asked %d times about an allowed command", n)
	}
	reqs := cls.seen()
	if len(reqs) != 1 {
		t.Fatalf("classifier requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r["command"] != "touch ran-1" || r["shell"] != "bash" || r["cwd"] != f.cwd || r["scenario"] != "repo" ||
		r["engine"] != "deepseek" || r["call_id"] != "call-1" || r["session_id"] == "" {
		t.Fatalf("classify request = %v", r)
	}
}

// A block verdict asks the parent and waits. The request carries the call, the
// verdict and the classifier's reason. An approval runs the command; a denial
// does not, and the model reads a refusal that does not repeat the reason.
func TestGuardBlockAsksTheParent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  func(ApprovalRequestParams) (any, *rpcError)
		wantRan bool
		wantMsg string
	}{
		{"approve", func(ApprovalRequestParams) (any, *rpcError) { return ApprovalAnswerResult{Decision: "approve"}, nil }, true, ""},
		{"deny", func(ApprovalRequestParams) (any, *rpcError) {
			return ApprovalAnswerResult{Decision: "deny", Message: "not in this repo"}, nil
		}, false, "not in this repo"},
		{"error is a denial", func(ApprovalRequestParams) (any, *rpcError) { return nil, errorf(CodeInternalError, "no card") }, false, ""},
		{"anything but approve denies", func(ApprovalRequestParams) (any, *rpcError) { return ApprovalAnswerResult{Decision: "yes"}, nil }, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cls := newFakeClassifier(t, 200, classifyBlock)
			f, rec := guardFixture(t,
				dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
				dsAnswer("All done."),
			)
			f.client.handshake(ClientCapabilities{Approvals: true})
			var approvals approvalClient
			approvals.install(f, tc.answer)

			var created CreateResult
			if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", cls.srv.URL+"/classify"), &created); rerr != nil {
				t.Fatalf("create: %v", rerr)
			}
			f.client.waitFor(NotifyResponseCompleted)

			asked := approvals.requests()
			if len(asked) != 1 {
				t.Fatalf("approval requests = %d, want 1", len(asked))
			}
			a := asked[0]
			if a.Type != "command_approval" || a.CallID != "call-1" || a.Command != "touch ran-1" || a.Cwd != f.cwd ||
				a.Verdict != "block" || a.Reason != "the command deletes the repository" || a.ResponseID == "" {
				t.Fatalf("approval request = %+v", a)
			}
			if got := ran(f, "ran-1"); got != tc.wantRan {
				t.Fatalf("command ran = %v, want %v", got, tc.wantRan)
			}
			_, bodies := rec.seen()
			last := bodies[len(bodies)-1]
			if tc.wantRan {
				return
			}
			if !strings.Contains(last, "Denied by the autonomy guard") {
				t.Fatalf("the model was not told the command was refused: %s", last)
			}
			if strings.Contains(last, "deletes the repository") {
				t.Fatal("the classifier's reason reached the model")
			}
			if tc.wantMsg != "" && !strings.Contains(last, tc.wantMsg) {
				t.Fatalf("the person's note did not reach the model: %s", last)
			}
		})
	}
}

// An endpoint that does not answer is not a verdict: the parent is asked, with
// verdict "unreachable", and nothing is refused on the endpoint's say alone.
func TestGuardUnreachableEndpointAsksTheParent(t *testing.T) {
	for name, cls := range map[string]*fakeClassifier{
		"status":      newFakeClassifier(t, 503, `{"verdict":"allow"}`),
		"not json":    newFakeClassifier(t, 200, `nope`),
		"no verdict":  newFakeClassifier(t, 200, `{"reason":"x"}`),
		"other value": newFakeClassifier(t, 200, `{"verdict":"maybe"}`),
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := guardFixture(t,
				dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
				dsAnswer("All done."),
			)
			f.client.handshake(ClientCapabilities{Approvals: true})
			var approvals approvalClient
			approvals.install(f, func(ApprovalRequestParams) (any, *rpcError) {
				return ApprovalAnswerResult{Decision: "approve"}, nil
			})
			if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", cls.srv.URL+"/classify"), &CreateResult{}); rerr != nil {
				t.Fatalf("create: %v", rerr)
			}
			f.client.waitFor(NotifyResponseCompleted)

			asked := approvals.requests()
			if len(asked) != 1 || asked[0].Verdict != "unreachable" || asked[0].Reason == "" {
				t.Fatalf("approval requests = %+v, want one with verdict unreachable and a reason", asked)
			}
			if !ran(f, "ran-1") {
				t.Fatal("an approved command did not run")
			}
		})
	}
}

// A classifier that cannot be reached at all (nothing listening) counts the
// same, and a parent that does not answer the request refuses the command.
func TestGuardNothingListeningAndNoAnswerRefuses(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL + "/classify"
	dead.Close()
	f, _ := guardFixture(t,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
		dsAnswer("All done."),
	)
	f.client.handshake(ClientCapabilities{Approvals: true})
	// onRequest stays nil: the client declines with method-not-found, which
	// is an error answer and so a denial.
	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", url), &CreateResult{}); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
	if ran(f, "ran-1") {
		t.Fatal("a command ran with the classifier down and nobody approving it")
	}
}

// The classifier never widens what the session's mode allows. Under readonly
// `bash` is refused before the guard is asked, so an allow verdict could not
// have helped and the endpoint is never called; a `deny` pattern is the same.
func TestGuardAllowNeverWidensTheMode(t *testing.T) {
	t.Run("readonly", func(t *testing.T) {
		cls := newFakeClassifier(t, 200, classifyAllow)
		f, _ := guardFixture(t,
			dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
			dsAnswer("All done."),
		)
		f.client.handshake(ClientCapabilities{Approvals: true})
		p := f.guardParams("run it", cls.srv.URL+"/classify")
		p.Harness.PermissionMode = "readonly"
		if rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr != nil {
			t.Fatalf("create: %v", rerr)
		}
		f.client.waitFor(NotifyResponseCompleted)
		if ran(f, "ran-1") {
			t.Fatal("bash ran under readonly")
		}
		if n := len(cls.seen()); n != 0 {
			t.Fatalf("the classifier was asked %d times about a call the mode refuses", n)
		}
	})
	t.Run("deny pattern", func(t *testing.T) {
		cls := newFakeClassifier(t, 200, classifyAllow)
		f, _ := guardFixture(t,
			dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
			dsAnswer("All done."),
		)
		f.client.handshake(ClientCapabilities{Approvals: true})
		p := f.guardParams("run it", cls.srv.URL+"/classify")
		p.Harness.Deny = []string{"touch"}
		if rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr != nil {
			t.Fatalf("create: %v", rerr)
		}
		f.client.waitFor(NotifyResponseCompleted)
		if ran(f, "ran-1") {
			t.Fatal("a denied command ran because the classifier allowed it")
		}
		if n := len(cls.seen()); n != 0 {
			t.Fatalf("the classifier was asked %d times about a denied command", n)
		}
	})
}

func TestGuardCreateRefusals(t *testing.T) {
	cases := []struct {
		name   string
		caps   ClientCapabilities
		mutate func(*CreateParams)
		code   int
		text   string
	}{
		{"no approvals capability", ClientCapabilities{}, func(*CreateParams) {}, CodeInvalidParams, "approvals"},
		{"scenario missing", ClientCapabilities{Approvals: true}, func(p *CreateParams) { p.Harness.Guard.Scenario = "" }, CodeInvalidParams, "scenario"},
		{"scenario unknown", ClientCapabilities{Approvals: true}, func(p *CreateParams) { p.Harness.Guard.Scenario = "lab" }, CodeInvalidParams, "scenario"},
		{"not a URL", ClientCapabilities{Approvals: true}, func(p *CreateParams) { p.Harness.Guard.ClassifyURL = "classifier" }, CodeInvalidParams, "classify_url"},
		{"ftp", ClientCapabilities{Approvals: true}, func(p *CreateParams) { p.Harness.Guard.ClassifyURL = "ftp://x/classify" }, CodeInvalidParams, "classify_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := guardFixture(t, dsAnswer("hi"))
			f.client.handshake(tc.caps)
			p := f.guardParams("run it", "http://127.0.0.1:1/classify")
			tc.mutate(&p)
			rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{})
			if rerr == nil || rerr.Code != tc.code || !strings.Contains(rerr.Message, tc.text) {
				t.Fatalf("create = %v, want code %d naming %q", rerr, tc.code, tc.text)
			}
		})
	}

	t.Run("a subcommand without the guard", func(t *testing.T) {
		f, _ := newDeepSeekFixture(t, dsAnswer("hi"))
		f.client.handshake(ClientCapabilities{Approvals: true})
		p := f.guardParams("run it", "http://127.0.0.1:1/classify")
		rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{})
		if rerr == nil || rerr.Code != CodeUnsupported || !strings.Contains(rerr.Message, "harness.guard") {
			t.Fatalf("create = %v, want %d naming harness.guard", rerr, CodeUnsupported)
		}
	})
}

// gatewayFixture is a DeepSeek session whose client starts pointed at nothing
// and with no key, so it can only reach the recorder through a route a create
// names.
func gatewayFixture(t *testing.T, hasKey bool, streams ...string) (*fixture, *deepseekRecorder, *providerhttp.Route) {
	t.Helper()
	route := &providerhttp.Route{}
	f, rec := newDeepSeekFixtureIn(t, t.TempDir(), t.TempDir(), func(r *session.Runner, o *Options) {
		client := deepseek.NewResponsesClient("http://127.0.0.1:1", "", deepseek.WithRoute(route))
		r.Client = client
		r.ClientFor = func(string) session.Client { return client }
		o.Routes = map[provider.Name]*providerhttp.Route{provider.DeepSeek: route}
		o.HasAPIKey = func(string) bool { return hasKey }
	}, streams...)
	return f, rec, route
}

// A create that names a base URL and says the gateway supplies the key runs
// against the gateway with no key in the process, and that is not the -32003 a
// keyless create gets otherwise. It fails if the client still sends to its own
// host, or the create is refused for the missing key.
func TestCreateRoutesProviderTrafficToTheGateway(t *testing.T) {
	f, rec, route := gatewayFixture(t, false, dsAnswer("hello"))
	f.client.handshake(ClientCapabilities{})
	p := f.createParams("hi")
	p.Model = testDeepSeekModel

	if rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr == nil || rerr.Code != CodeCredentialsMissing {
		t.Fatalf("a keyless create with no gateway = %v, want -32003", rerr)
	}

	p.Harness.Providers = map[string]ProviderRoute{"deepseek": {BaseURL: rec.url + "/gw/", GatewaySuppliesKey: true}}
	if rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr != nil {
		t.Fatalf("create through a gateway: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)

	paths, _ := rec.seen()
	if len(paths) == 0 || !strings.HasPrefix(paths[0], "/gw/") {
		t.Fatalf("the provider saw paths %v, want them under /gw/", paths)
	}
	if !route.GatewayKey() {
		t.Fatal("the route does not record that the gateway supplies the key")
	}
}

func TestCreateRefusesBadGatewayFields(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*CreateHarness)
		text   string
	}{
		"unknown provider": {func(h *CreateHarness) { h.Providers = map[string]ProviderRoute{"openai": {}} }, "providers.openai"},
		"bad base URL":     {func(h *CreateHarness) { h.Providers = map[string]ProviderRoute{"deepseek": {BaseURL: "gateway"}} }, "base_url"},
		"bad proxy": {func(h *CreateHarness) {
			p := "ftp://proxy:1"
			h.BrowserProxy = &p
		}, "browser_proxy"},
	} {
		t.Run(name, func(t *testing.T) {
			f, _, route := gatewayFixture(t, true, dsAnswer("hi"))
			f.client.handshake(ClientCapabilities{})
			p := f.createParams("hi")
			p.Model = testDeepSeekModel
			tc.mutate(p.Harness)
			rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{})
			if rerr == nil || rerr.Code != CodeInvalidParams || !strings.Contains(rerr.Message, tc.text) {
				t.Fatalf("create = %v, want -32602 naming %q", rerr, tc.text)
			}
			if route.GatewayKey() {
				t.Fatal("a refused create changed the route")
			}
		})
	}
}

// A run cancelled while the parent is still deciding does not leave the call
// waiting, and the command never runs: an ask fails closed on an interrupt.
func TestGuardCancelWhileAskingRefusesTheCommand(t *testing.T) {
	cls := newFakeClassifier(t, 200, classifyBlock)
	f, _ := guardFixture(t,
		dsToolCall("call-1", "bash", `{"command":"touch ran-1"}`),
		dsAnswer("All done."),
	)
	f.client.handshake(ClientCapabilities{Approvals: true})
	asked := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var approvals approvalClient
	approvals.install(f, func(ApprovalRequestParams) (any, *rpcError) {
		close(asked)
		<-release
		return ApprovalAnswerResult{Decision: "approve"}, nil
	})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.guardParams("run it", cls.srv.URL+"/classify"), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	<-asked
	if rerr := f.client.call(MethodResponsesCancel, IDParams{ResponseID: created.Response.ID}, nil); rerr != nil {
		t.Fatalf("cancel: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
	if ran(f, "ran-1") {
		t.Fatal("the command ran after the run was cancelled mid-ask")
	}
}

// claude-session speaks Managed Agents, which has no request for the parent to
// answer about one call, so a create naming harness.guard is refused rather
// than starting a session whose blocked commands could not be asked about.
func TestManagedAgentsRefusesAGuardSetting(t *testing.T) {
	f, _ := newManagedAgentsFixture(t, 0, "")
	f.client.handshake(ClientCapabilities{Approvals: true})
	p := maCreateParamsFor(f, "hi", nil)
	p.Harness.Guard = &GuardConfig{ClassifyURL: "http://127.0.0.1:1/classify", Scenario: "repo"}
	rerr := f.client.call(MethodSessionsCreate, p, &maCreateResult{})
	if rerr == nil || rerr.Code != CodeUnsupported || !strings.Contains(rerr.Message, "harness.guard") {
		t.Fatalf("create = %v, want %d naming harness.guard", rerr, CodeUnsupported)
	}
}
