// Package guardtest stands up fake stage 1 and stage 2 servers for tests that
// run a whole session through the autonomy guard.
package guardtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Behaviour is what the fake servers answer with. Change it with Fake.Set.
type Behaviour struct {
	// PBlock is stage 1's probability of block.
	PBlock float64
	// Stage1Status, when not 200, fails every stage 1 request with it.
	Stage1Status int
	// Verdict and Reason are stage 2's answer.
	Verdict string
	Reason  string
	// Stage2Status, when not 200, fails every stage 2 request with it.
	Stage2Status int
	// HoldStage1, when set, makes stage 1 wait until it is closed before
	// answering. Fake.Stage1Arrived is signalled when a request reaches it.
	HoldStage1 chan struct{}
	// HoldCommand holds the stage 1 request for one proposed command until
	// its channel closes.
	HoldCommand map[string]chan struct{}
}

// Fake is a pair of servers and the requests they saw.
type Fake struct {
	Stage1URL string
	Stage2URL string

	// Stage1Arrived receives one value per stage 1 request, as it arrives.
	Stage1Arrived chan struct{}

	mu     sync.Mutex
	b      Behaviour
	stage1 []string
	stage2 []string
}

// New starts both servers. By default stage 1 says block with probability
// 0.9, which sends every command to stage 2, and stage 2 allows.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{
		b:             Behaviour{PBlock: 0.9, Verdict: "allow", Reason: "a routine command"},
		Stage1Arrived: make(chan struct{}, 64),
	}
	s1 := httptest.NewServer(http.HandlerFunc(f.serveStage1))
	s2 := httptest.NewServer(http.HandlerFunc(f.serveStage2))
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)
	f.Stage1URL, f.Stage2URL = s1.URL, s2.URL
	return f
}

// Set changes the behaviour for the requests that follow.
func (f *Fake) Set(change func(*Behaviour)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.b)
}

// Stage1Requests is the bodies stage 1 received.
func (f *Fake) Stage1Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stage1...)
}

// Stage2Requests is the bodies stage 2 received.
func (f *Fake) Stage2Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stage2...)
}

func (f *Fake) serveStage1(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		State struct {
			ProposedCommand string `json:"proposed_command"`
		} `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	_ = json.Unmarshal(body, &req)
	question := Question
	for name := range req.Questions {
		question = name
	}

	f.mu.Lock()
	f.stage1 = append(f.stage1, string(body))
	b := f.b
	hold := b.HoldStage1
	if c, ok := b.HoldCommand[req.State.ProposedCommand]; ok {
		hold = c
	}
	f.mu.Unlock()

	select {
	case f.Stage1Arrived <- struct{}{}:
	default:
	}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if b.Stage1Status != 0 && b.Stage1Status != http.StatusOK {
		http.Error(w, "stage 1 down", b.Stage1Status)
		return
	}
	fmt.Fprintf(w, `{"answers":{%q:{"choice":"block","probabilities":{"allow":%g,"block":%g}}}}`, question, 1-b.PBlock, b.PBlock)
}

func (f *Fake) serveStage2(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.stage2 = append(f.stage2, string(body))
	b := f.b
	f.mu.Unlock()
	if b.Stage2Status != 0 && b.Stage2Status != http.StatusOK {
		http.Error(w, "stage 2 down", b.Stage2Status)
		return
	}
	verdict, _ := json.Marshal(map[string]string{"reason": b.Reason, "decision": b.Verdict})
	resp, _ := json.Marshal(map[string]any{
		"stop_reason": "end_turn",
		"content":     []map[string]any{{"type": "text", "text": string(verdict)}},
		"usage":       map[string]int{"input_tokens": 120, "output_tokens": 30, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}
