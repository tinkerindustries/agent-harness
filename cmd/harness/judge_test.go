package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/kimi"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// judgeFakeStore is the settings surface of *store.Store, backed by a map so
// newJudge's model resolution is testable without a database (TESTING.md:
// small interfaces satisfied by a struct declared in the test file).
type judgeFakeStore struct {
	values map[string]string
}

func (f *judgeFakeStore) Setting(_ context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *judgeFakeStore) SetSetting(_ context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *judgeFakeStore) DeleteSetting(_ context.Context, key string) error {
	delete(f.values, key)
	return nil
}

// fakeCompletionEndpoint stands in for one provider's /chat/completions: it
// records every request and answers with a valid verdict, so a test can tell
// which client carried a judge's request from which endpoint saw it.
type fakeCompletionEndpoint struct {
	mu    sync.Mutex
	hits  int
	model string
}

func (f *fakeCompletionEndpoint) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q, want /chat/completions", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.hits++
		f.model = body.Model
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"{\"score\": 4, \"completed\": true, \"reasoning\": \"did the work\"}"},"finish_reason":"stop"}]}`, body.Model)
	}
}

func (f *fakeCompletionEndpoint) count() (hits int, model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, f.model
}

// judgeTranscript is a minimal event log RenderTranscript accepts: one
// content delta, so a judge call has something to read.
var judgeTranscript = []store.Event{
	{Kind: store.KindContentDelta, Payload: json.RawMessage(`{"text":"the agent did the work"}`)},
}

// newJudgeEndpoints builds a DeepSeek fake and a Kimi fake, each a fresh
// httptest server with its own counters, and clients pointed at them, plus a
// Gemini client with no fake behind it. The caller closes both servers.
// Nothing here exercises the Gemini client over HTTP — its request and
// response shapes are POST /v1beta/interactions, not
// fakeCompletionEndpoint's OpenAI-format /chat/completions, so
// TestNewJudgeRoutesByModel's Gemini case only pins which client newJudge
// selects, not a round trip through it.
func newJudgeEndpoints(t *testing.T) (deepSeekEP, kimiEP *fakeCompletionEndpoint, deepSeekClient *deepseek.Client, kimiClient *kimi.Client, geminiClient *gemini.Client) {
	t.Helper()
	deepSeekEP = &fakeCompletionEndpoint{}
	kimiEP = &fakeCompletionEndpoint{}
	deepSeekSrv := httptest.NewServer(deepSeekEP.handler(t))
	t.Cleanup(deepSeekSrv.Close)
	kimiSrv := httptest.NewServer(kimiEP.handler(t))
	t.Cleanup(kimiSrv.Close)
	return deepSeekEP, kimiEP, deepseek.NewClient(deepSeekSrv.URL, "sk-test"), kimi.NewClient(kimiSrv.URL, "sk-test"), gemini.NewClient(gemini.DefaultBaseURL)
}

// TestNewJudgeRoutesByModel pins that the judge's client is resolved through
// the model→provider table, asserted by which fake endpoint the request
// reaches: a kimi-k3 judge speaks to the Kimi client and a deepseek-v4-pro
// judge to the DeepSeek one, each scoring a real verdict end to end.
func TestNewJudgeRoutesByModel(t *testing.T) {
	res := settings.NewResolver(&judgeFakeStore{values: map[string]string{}})
	ctx := context.Background()

	cases := []struct {
		model      string
		wantClient string
	}{
		{model: "kimi-k3", wantClient: "the Kimi client"},
		{model: "deepseek-v4-pro", wantClient: "the DeepSeek client"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			deepSeekEP, kimiEP, deepSeekClient, kimiClient, geminiClient := newJudgeEndpoints(t)
			judge, err := newJudge(ctx, res, tc.model, deepSeekClient, kimiClient, geminiClient)
			if err != nil {
				t.Fatalf("newJudge(%q): %v", tc.model, err)
			}
			if judge.Model != tc.model {
				t.Fatalf("judge.Model = %q, want %q", judge.Model, tc.model)
			}
			verdict, err := judge.Score(ctx, "rubric", judgeTranscript)
			if err != nil {
				t.Fatalf("judge.Score: %v", err)
			}
			if verdict == nil || verdict.Score != 4 || !verdict.Completed {
				t.Fatalf("verdict = %+v, want score 4 completed", verdict)
			}
			wantEP, otherEP := kimiEP, deepSeekEP
			if tc.model == "deepseek-v4-pro" {
				wantEP, otherEP = deepSeekEP, kimiEP
			}
			if hits, model := wantEP.count(); hits != 1 || model != tc.model {
				t.Errorf("%s saw %d request(s) for model %q, want 1 for %q", tc.wantClient, hits, model, tc.model)
			}
			if hits, _ := otherEP.count(); hits != 0 {
				t.Errorf("the other endpoint saw %d request(s), want 0", hits)
			}
		})
	}
}

// TestNewJudgeRoutesGeminiModel pins the Gemini case of the same routing
// table, separately from TestNewJudgeRoutesByModel above: internal/gemini's
// request and response shapes are POST /v1beta/interactions, not
// fakeCompletionEndpoint's OpenAI-format /chat/completions, so there is no
// fake to score a real verdict through here. What matters — that a
// gemini-3.7-flash judge gets the Gemini client rather than silently falling
// back to DeepSeek's, the regression newJudge's own doc comment warns about —
// is provable by identity alone.
func TestNewJudgeRoutesGeminiModel(t *testing.T) {
	res := settings.NewResolver(&judgeFakeStore{values: map[string]string{}})
	ctx := context.Background()
	_, _, deepSeekClient, kimiClient, geminiClient := newJudgeEndpoints(t)

	judge, err := newJudge(ctx, res, "gemini-3.7-flash", deepSeekClient, kimiClient, geminiClient)
	if err != nil {
		t.Fatalf("newJudge(gemini-3.7-flash): %v", err)
	}
	if judge.Model != "gemini-3.7-flash" {
		t.Fatalf("judge.Model = %q, want gemini-3.7-flash", judge.Model)
	}
	if judge.Client != geminiClient {
		t.Errorf("judge.Client is not the Gemini client passed to newJudge")
	}
}

// TestNewJudgeResolvesJudgeModelSetting pins the default and the override: an
// eval that names no judge model gets model.judge — kimi-k3 when the setting
// is unset, the stored value when it is — and the resolved model's client is
// the one that carries the request.
func TestNewJudgeResolvesJudgeModelSetting(t *testing.T) {
	ctx := context.Background()

	t.Run("unset resolves to kimi-k3", func(t *testing.T) {
		deepSeekEP, kimiEP, deepSeekClient, kimiClient, geminiClient := newJudgeEndpoints(t)
		res := settings.NewResolver(&judgeFakeStore{values: map[string]string{}})
		judge, err := newJudge(ctx, res, "", deepSeekClient, kimiClient, geminiClient)
		if err != nil {
			t.Fatalf("newJudge: %v", err)
		}
		if judge.Model != "kimi-k3" {
			t.Fatalf("judge.Model = %q, want kimi-k3 (the model.judge default)", judge.Model)
		}
		if _, err := judge.Score(ctx, "rubric", judgeTranscript); err != nil {
			t.Fatalf("judge.Score: %v", err)
		}
		if hits, model := kimiEP.count(); hits != 1 || model != "kimi-k3" {
			t.Errorf("Kimi endpoint saw %d request(s) for %q, want 1 for kimi-k3", hits, model)
		}
		if hits, _ := deepSeekEP.count(); hits != 0 {
			t.Errorf("DeepSeek endpoint saw %d request(s), want 0", hits)
		}
	})

	t.Run("stored override wins", func(t *testing.T) {
		deepSeekEP, kimiEP, deepSeekClient, kimiClient, geminiClient := newJudgeEndpoints(t)
		res := settings.NewResolver(&judgeFakeStore{values: map[string]string{
			settings.KeyJudgeModel: "deepseek-v4-flash",
		}})
		judge, err := newJudge(ctx, res, "", deepSeekClient, kimiClient, geminiClient)
		if err != nil {
			t.Fatalf("newJudge: %v", err)
		}
		if judge.Model != "deepseek-v4-flash" {
			t.Fatalf("judge.Model = %q, want deepseek-v4-flash", judge.Model)
		}
		if _, err := judge.Score(ctx, "rubric", judgeTranscript); err != nil {
			t.Fatalf("judge.Score: %v", err)
		}
		if hits, model := deepSeekEP.count(); hits != 1 || model != "deepseek-v4-flash" {
			t.Errorf("DeepSeek endpoint saw %d request(s) for %q, want 1 for deepseek-v4-flash", hits, model)
		}
		if hits, _ := kimiEP.count(); hits != 0 {
			t.Errorf("Kimi endpoint saw %d request(s), want 0", hits)
		}
	})
}

// TestNewJudgeRejectsUnknownModel pins that a judge model outside the
// model→provider table fails loudly instead of silently falling back to a
// provider (docs/KIMI-INTEGRATION.md §4.3): the eval must not start with a
// judge that would score with the wrong account, or with no judge at all.
func TestNewJudgeRejectsUnknownModel(t *testing.T) {
	deepSeekSrv := httptest.NewServer(http.NotFoundHandler())
	defer deepSeekSrv.Close()
	kimiSrv := httptest.NewServer(http.NotFoundHandler())
	defer kimiSrv.Close()

	res := settings.NewResolver(&judgeFakeStore{values: map[string]string{}})
	ctx := context.Background()
	_, err := newJudge(ctx, res, "no-such-model",
		deepseek.NewClient(deepSeekSrv.URL, "sk-test"), kimi.NewClient(kimiSrv.URL, "sk-test"), gemini.NewClient(gemini.DefaultBaseURL))
	if err == nil {
		t.Fatal("newJudge(no-such-model) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Errorf("error = %q, want it to name the unknown model", err)
	}
}
