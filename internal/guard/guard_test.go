package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL + "/classify"}
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

var sample = Request{
	Command: "rm -rf build", Shell: "bash", CWD: "/work", Branch: "main", Scenario: "repo",
	Engine: "deepseek", SessionID: "sess_1", CallID: "call_1",
}

// The request is the memory's contract field for field, as JSON, posted to the
// URL the parent gave, and an allow answer is an allow.
func TestClassifySendsTheContractAndReadsAnAllow(t *testing.T) {
	var got map[string]any
	var method, contentType string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		method, contentType = r.Method, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"verdict":"allow","reason":"","decided_by":"rules","guard_version":"v1"}`))
	})
	res := c.Classify(context.Background(), sample)
	if res.Verdict != Allow || res.DecidedBy != "rules" || res.GuardVersion != "v1" {
		t.Fatalf("result = %+v", res)
	}
	if method != http.MethodPost || contentType != "application/json" {
		t.Errorf("method %q, content type %q", method, contentType)
	}
	for k, want := range map[string]string{
		"command": "rm -rf build", "shell": "bash", "cwd": "/work", "branch": "main", "scenario": "repo",
		"engine": "deepseek", "session_id": "sess_1", "call_id": "call_1",
	} {
		if got[k] != want {
			t.Errorf("request %s = %v, want %q", k, got[k], want)
		}
	}
	// A request with no branch omits the key rather than sending "".
	var raw map[string]any
	b, _ := json.Marshal(Request{Command: "ls"})
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["branch"]; ok {
		t.Error("an empty branch is sent")
	}
}

func TestClassifyReadsABlockAndBoundsItsReason(t *testing.T) {
	long := strings.Repeat("é", 5000)
	c := serve(t, reply(200, `{"verdict":"block","reason":"`+long+`","decided_by":"stage2"}`))
	res := c.Classify(context.Background(), sample)
	if res.Verdict != Block {
		t.Fatalf("verdict = %q", res.Verdict)
	}
	if n := len([]rune(res.Reason)); n > maxReasonRunes+1 {
		t.Errorf("the reason is %d runes, want it cut to %d", n, maxReasonRunes)
	}
}

// Everything that is not a clean 200 with a verdict is unreachable, and says
// why without repeating the body or the URL.
func TestClassifyTreatsEverythingElseAsUnreachable(t *testing.T) {
	huge := `{"verdict":"allow","reason":"` + strings.Repeat("x", maxAnswerBytes) + `"}`
	for name, h := range map[string]http.HandlerFunc{
		"500":              reply(500, `{"verdict":"allow"}`),
		"404":              reply(404, `{"verdict":"allow"}`),
		"204":              reply(204, ``),
		"not json":         reply(200, `<html>secret-body</html>`),
		"no verdict":       reply(200, `{"reason":"x"}`),
		"unknown verdict":  reply(200, `{"verdict":"escalate"}`),
		"oversized answer": reply(200, huge),
	} {
		t.Run(name, func(t *testing.T) {
			res := serve(t, h).Classify(context.Background(), sample)
			if res.Verdict != Unreachable || res.Err == "" {
				t.Fatalf("result = %+v, want unreachable with a reason", res)
			}
			if strings.Contains(res.Err, "secret-body") || strings.Contains(res.Err, "127.0.0.1") {
				t.Errorf("the reason repeats the body or the URL: %q", res.Err)
			}
		})
	}
}

func TestClassifyTimesOutAsUnreachable(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	})
	c.Timeout = 50 * time.Millisecond
	start := time.Now()
	res := c.Classify(context.Background(), sample)
	if res.Verdict != Unreachable || !strings.Contains(res.Err, "no answer within") {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %s to give up on a 50ms deadline", time.Since(start))
	}
}

func TestClassifyNothingListeningIsUnreachable(t *testing.T) {
	c := &Client{URL: "http://127.0.0.1:1/classify"}
	if res := c.Classify(context.Background(), sample); res.Verdict != Unreachable {
		t.Fatalf("result = %+v", res)
	}
}

func TestBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	if got := Branch(context.Background(), dir); got != "" {
		t.Errorf("a directory outside a repository has branch %q", got)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "topic"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if got := Branch(context.Background(), dir); got != "topic" {
		t.Errorf("branch = %q, want topic", got)
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("detach: %v %s", err, out)
	}
	if got := Branch(context.Background(), dir); got != "" {
		t.Errorf("a detached HEAD has branch %q, want none", got)
	}
}
