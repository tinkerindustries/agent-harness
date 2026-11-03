package guard

import (
	"context"
	"encoding/json"
	"github.com/mrgeoffrich/agent-harness/internal/guard/guardtest"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func stage2Server(t *testing.T, handler http.HandlerFunc) *Stage2Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewStage2Client(Stage2Config{Bundle: mustBundle(t), BaseURL: srv.URL, APIKey: "sk-test", Timeout: 2 * time.Second, RetryBase: time.Millisecond, RetryMax: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func messageBody(stop, text string) string {
	b, _ := json.Marshal(map[string]any{
		"stop_reason": stop,
		"content":     []map[string]any{{"type": "thinking", "thinking": "hm"}, {"type": "text", "text": text}},
		"usage":       map[string]int{"input_tokens": 11, "output_tokens": 22, "cache_creation_input_tokens": 3, "cache_read_input_tokens": 4},
	})
	return string(b)
}

func TestStage2Verdict(t *testing.T) {
	var gotKey, gotVersion, gotPath string
	var req map[string]any
	c := stage2Server(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVersion, gotPath = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		io.WriteString(w, messageBody("end_turn", `{"reason":"inside the sandbox","decision":"allow"}`))
	})
	res, err := c.Review(context.Background(), State{ProposedCommand: "go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != "allow" || res.Reason != "inside the sandbox" {
		t.Fatalf("res = %+v", res)
	}
	if res.Usage == nil || *res.Usage != (Usage{InputTokens: 11, OutputTokens: 22, CacheCreationInputTokens: 3, CacheReadInputTokens: 4}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if gotKey != "sk-test" || gotVersion != "2023-06-01" || gotPath != "/v1/messages" {
		t.Fatalf("key=%q version=%q path=%q", gotKey, gotVersion, gotPath)
	}
	if req["model"] != guardtest.Stage2Model || req["max_tokens"] != float64(1234) {
		t.Fatalf("request = %v", req)
	}
	if _, ok := req["temperature"]; ok {
		t.Fatal("temperature sent")
	}
}

func TestStage2Failures(t *testing.T) {
	good := `{"reason":"r","decision":"allow"}`
	cases := map[string]string{
		"max_tokens stop":  messageBody("max_tokens", good),
		"refusal stop":     messageBody("refusal", good),
		"pause_turn stop":  messageBody("pause_turn", good),
		"no stop reason":   `{"content":[{"type":"text","text":"{\"reason\":\"r\",\"decision\":\"allow\"}"}]}`,
		"extra key":        messageBody("end_turn", `{"reason":"r","decision":"allow","confidence":1}`),
		"missing reason":   messageBody("end_turn", `{"decision":"allow"}`),
		"missing decision": messageBody("end_turn", `{"reason":"r"}`),
		"null reason":      messageBody("end_turn", `{"reason":null,"decision":"allow"}`),
		"bad decision":     messageBody("end_turn", `{"reason":"r","decision":"maybe"}`),
		"decision number":  messageBody("end_turn", `{"reason":"r","decision":1}`),
		"reason number":    messageBody("end_turn", `{"reason":5,"decision":"allow"}`),
		"not an object":    messageBody("end_turn", `"allow"`),
		"prose":            messageBody("end_turn", `allow`),
		"trailing text":    messageBody("end_turn", good+` extra`),
		"empty text":       messageBody("end_turn", ``),
		"no content":       `{"stop_reason":"end_turn","content":[]}`,
		"not json":         `oops`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := stage2Server(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if res, err := c.Review(context.Background(), State{}); err == nil {
				t.Fatalf("accepted: %+v", res)
			}
		})
	}
}

func TestStage2ReturnsUsageOnInvalidVerdict(t *testing.T) {
	c := stage2Server(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, messageBody("max_tokens", `{"reason":"cut`))
	})
	res, err := c.Review(context.Background(), State{})
	if err == nil || res.Usage == nil || res.Usage.OutputTokens != 22 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestStage2HTTPErrorAndRetry(t *testing.T) {
	var calls atomic.Int32
	c := stage2Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"overloaded"}`, 529)
	})
	if _, err := c.Review(context.Background(), State{}); err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	calls.Store(0)
	c = stage2Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad key", http.StatusUnauthorized)
	})
	if _, err := c.Review(context.Background(), State{}); err == nil || calls.Load() != 1 {
		t.Fatalf("401: err=%v calls=%d", err, calls.Load())
	}
}

func TestStage2RetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	c := stage2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, messageBody("end_turn", `{"reason":"r","decision":"block"}`))
	})
	res, err := c.Review(context.Background(), State{})
	if err != nil || res.Decision != "block" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestStage2TimeoutCoversRetries(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c, err := NewStage2Client(Stage2Config{Bundle: mustBundle(t), BaseURL: srv.URL, APIKey: "k", Timeout: 100 * time.Millisecond, MaxRetries: 3, RetryBase: time.Millisecond, RetryMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Review(context.Background(), State{}); err == nil {
		t.Fatal("timeout accepted")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %v with a 100ms deadline", took)
	}
}

func TestStage2NeedsKey(t *testing.T) {
	if _, err := NewStage2Client(Stage2Config{Bundle: mustBundle(t)}); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestStage2SendsTheBundlesContent(t *testing.T) {
	bundle := mustBundle(t, func(b map[string]any) {
		section(b, "stage2")["state_fields"] = []any{"proposed_command", "scripts_run", "working_directory"}
	})
	c, err := NewStage2Client(Stage2Config{Bundle: bundle, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.request(State{WorkingDirectory: "/w", Branch: "b", ProposedCommand: "ls", PackageScripts: []PackageScriptEntry{{Runner: "npm"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"` + guardtest.Stage2Model + `","max_tokens":1234,` +
		`"system":[{"type":"text","text":"Invented system prompt with <angle> & ampersand.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":"{\n  \"proposed_command\": \"ls\",\n  \"scripts_run\": [],\n  \"working_directory\": \"/w\"\n}"}],` +
		`"output_config":{"effort":"low","format":{"type":"json_schema","schema":` + string(bundle.stage2.schema) + `}}}`
	if string(body) != want {
		t.Fatalf("body = %s\nwant   %s", body, want)
	}
}

func TestStage2NeedsABundle(t *testing.T) {
	if _, err := NewStage2Client(Stage2Config{APIKey: "k"}); err == nil {
		t.Fatal("no bundle accepted")
	}
}
