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

func stage1Server(t *testing.T, handler http.HandlerFunc) *Stage1Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewStage1Client(Stage1Config{Bundle: mustBundle(t), Endpoint: srv.URL, Timeout: 2 * time.Second, RetryBase: time.Millisecond, RetryMax: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func answer(allow, block string, choice string) string {
	return `{"answers":{"gate_check":{"choice":"` + choice + `","probabilities":{"allow":` + allow + `,"block":` + block + `}}}}`
}

func TestStage1RequestAndValidAnswer(t *testing.T) {
	var got map[string]json.RawMessage
	var path string
	c := stage1Server(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, answer("0.9", "0.1", "allow"))
	})
	state := State{WorkingDirectory: "/w", Branch: "b", ProposedCommand: "ls", PackageScripts: []PackageScriptEntry{{Runner: "npm", Script: "x"}}}
	res, err := c.PBlock(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if res.PBlock != 0.1 {
		t.Fatalf("pblock = %v", res.PBlock)
	}
	if path != "/v1/systemone" {
		t.Fatalf("path = %s", path)
	}
	if string(got["model"]) != `"`+guardtest.Stage1Model+`"` || string(got["keep_alive"]) != `"30m"` {
		t.Fatalf("model/keep_alive = %s %s", got["model"], got["keep_alive"])
	}
	var st map[string]json.RawMessage
	_ = json.Unmarshal(got["state"], &st)
	if len(st) != 5 {
		t.Fatalf("stage 1 state has %d fields: %s", len(st), got["state"])
	}
	if _, ok := st["package_scripts"]; ok {
		t.Fatal("stage 1 state carries package_scripts")
	}
	if string(st["recent_commands"]) != "[]" || string(st["scripts_run"]) != "[]" {
		t.Fatalf("lists = %s %s", st["recent_commands"], st["scripts_run"])
	}
	if want := mustBundle(t).stage1.questions; string(got["questions"]) != string(want) {
		t.Fatal("questions differ from the bundle's")
	}
}

func TestStage1Failures(t *testing.T) {
	cases := map[string]string{
		"wrong labels":  `{"answers":{"gate_check":{"choice":"allow","probabilities":{"allow":0.5,"deny":0.5}}}}`,
		"three labels":  `{"answers":{"gate_check":{"choice":"allow","probabilities":{"allow":0.5,"block":0.4,"other":0.1}}}}`,
		"one label":     `{"answers":{"gate_check":{"choice":"allow","probabilities":{"allow":1}}}}`,
		"null value":    `{"answers":{"gate_check":{"choice":"allow","probabilities":{"allow":null,"block":1}}}}`,
		"string value":  `{"answers":{"gate_check":{"choice":"allow","probabilities":{"allow":"0.5","block":0.5}}}}`,
		"above one":     answer("-0.5", "1.5", "block"),
		"below zero":    answer("1.2", "-0.2", "allow"),
		"sum too high":  answer("0.6", "0.42", "allow"),
		"sum too low":   answer("0.4", "0.58", "allow"),
		"bad choice":    answer("0.9", "0.1", "maybe"),
		"no question":   `{"answers":{}}`,
		"not json":      `<html>`,
		"missing probs": `{"answers":{"gate_check":{"choice":"allow"}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := stage1Server(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if res, err := c.PBlock(context.Background(), State{}); err == nil {
				t.Fatalf("accepted %s: %+v", body, res)
			}
		})
	}
}

func TestStage1SumWithinTolerance(t *testing.T) {
	for _, body := range []string{answer("0.5", "0.51", "allow"), answer("0.495", "0.5", "allow"), answer("1", "0", "allow")} {
		c := stage1Server(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
		if _, err := c.PBlock(context.Background(), State{}); err != nil {
			t.Fatalf("rejected %s: %v", body, err)
		}
	}
}

func TestStage1HTTPErrorAndRetry(t *testing.T) {
	var calls atomic.Int32
	c := stage1Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := c.PBlock(context.Background(), State{}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want one retry", calls.Load())
	}
	calls.Store(0)
	c = stage1Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "nope", http.StatusBadRequest)
	})
	if _, err := c.PBlock(context.Background(), State{}); err == nil {
		t.Fatal("400 accepted")
	}
	if calls.Load() != 1 {
		t.Fatalf("400 retried: %d calls", calls.Load())
	}
}

func TestStage1RetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	c := stage1Server(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, answer("0.2", "0.8", "block"))
	})
	res, err := c.PBlock(context.Background(), State{})
	if err != nil || res.PBlock != 0.8 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestStage1TimeoutCoversRetries(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c, err := NewStage1Client(Stage1Config{Bundle: mustBundle(t), Endpoint: srv.URL, Timeout: 100 * time.Millisecond, MaxRetries: 3, RetryBase: time.Millisecond, RetryMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.PBlock(context.Background(), State{}); err == nil {
		t.Fatal("timeout accepted")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %v with a 100ms deadline", took)
	}
}

func TestStage1NeedsEndpoint(t *testing.T) {
	if _, err := NewStage1Client(Stage1Config{Bundle: mustBundle(t)}); err == nil {
		t.Fatal("empty endpoint accepted")
	}
}

func TestStage1SendsOnlyTheListedFieldsInOrder(t *testing.T) {
	var got map[string]json.RawMessage
	bundle := mustBundle(t, func(b map[string]any) {
		section(b, "stage1")["state_fields"] = []any{"recent_commands", "proposed_command", "branch"}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, answer("0.9", "0.1", "allow"))
	}))
	defer srv.Close()
	c, err := NewStage1Client(Stage1Config{Bundle: bundle, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PBlock(context.Background(), State{WorkingDirectory: "/w", Branch: "b", ProposedCommand: "ls", ScriptsRun: []ScriptEntry{{Path: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if want := `{"recent_commands":[],"proposed_command":"ls","branch":"b"}`; string(got["state"]) != want {
		t.Fatalf("state = %s", got["state"])
	}
}
