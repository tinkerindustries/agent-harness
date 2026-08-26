package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/evals"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// seedEval records a two-arm run whose base arm searched through Bash and
// whose other arm used the tools, which is the comparison the whole surface
// exists to render.
func seedEval(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	run := store.EvalRun{
		ID: id, Suite: "search", SuiteJSON: json.RawMessage(`{"name":"search"}`),
		Variants: []string{"base", "search-first"}, Replicates: 2,
		JudgeModel: "deepseek-v4-pro", Note: "does naming the tools help",
		Status:    store.EvalStatusOK,
		StartedAt: time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC),
		FinishedAt: func() *time.Time {
			t := time.Date(2026, 8, 13, 4, 30, 0, 0, time.UTC)
			return &t
		}(),
	}
	var members []store.EvalMember
	for i, spec := range []struct {
		variant string
		share   float64
	}{
		{"base", 0.10}, {"base", 0.20},
		{"search-first", 0.80}, {"search-first", 0.90},
	} {
		members = append(members, store.EvalMember{
			EvalRunID: id, RequestID: "eval-" + string(rune('a'+i)),
			TaskID: "task-a", Variant: spec.variant, Replicate: 1 + i%2,
			SessionID: "sess-" + string(rune('a'+i)), Status: "ok",
			Scores:   json.RawMessage(`{"search_via_tool":` + formatFloat(spec.share) + `}`),
			Verdict:  json.RawMessage(`{"score":4,"completed":true}`),
			CostUSD:  0.02,
			SubTurns: 30,
		})
	}
	if err := st.CreateEvalRun(ctx, run, members); err != nil {
		t.Fatalf("CreateEvalRun: %v", err)
	}
}

// getJSON fetches a 200 and decodes it, returning the raw body so a test can
// also assert the encoding (an empty list must be [] and not null).
func getJSON(t *testing.T, url string, into any) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("decode %s: %v (body %s)", url, err, body)
		}
	}
	return body
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestListEvalsCarriesCountsAndAHeadline(t *testing.T) {
	srv, st, _ := newTestServer(t)
	seedEval(t, st, "evr-1")

	var rows []evalRunRow
	getJSON(t, srv.URL+"/api/evals", &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Total != 4 || got.Finished != 4 || got.Failed != 0 {
		t.Errorf("counts = total %d finished %d failed %d", got.Total, got.Finished, got.Failed)
	}
	if got.CostUSD < 0.079 || got.CostUSD > 0.081 {
		t.Errorf("cost = %v, want the members summed", got.CostUSD)
	}
	if got.Headline == nil {
		t.Fatal("no headline delta on the list row")
	}
	if got.Headline.Metric != "search_via_tool" {
		t.Errorf("headline metric = %q", got.Headline.Metric)
	}
	if got.Headline.Diff < 0.69 || got.Headline.Diff > 0.71 {
		t.Errorf("headline diff = %v, want ~0.70", got.Headline.Diff)
	}
	if !got.Headline.Significant {
		t.Error("a 70-point difference with tight spread should be marked significant")
	}
}

func TestGetEvalCarriesMembersSummaryAndDeltas(t *testing.T) {
	srv, st, _ := newTestServer(t)
	seedEval(t, st, "evr-1")

	var detail evalRunDetail
	getJSON(t, srv.URL+"/api/evals/evr-1", &detail)
	if len(detail.Members) != 4 {
		t.Fatalf("members = %d, want 4", len(detail.Members))
	}
	if detail.Members[0].Verdict == nil || detail.Members[0].Verdict.Score != 4 {
		t.Errorf("member verdict = %+v", detail.Members[0].Verdict)
	}
	if detail.Members[0].Scores["search_via_tool"] == 0 {
		t.Error("member scores did not survive the round trip")
	}

	var perVariant int
	for _, s := range detail.Summary {
		if s.Metric == "search_via_tool" {
			perVariant++
			if s.N != 2 {
				t.Errorf("summary %s/%s n = %d, want 2", s.Metric, s.Variant, s.N)
			}
		}
	}
	if perVariant != 2 {
		t.Errorf("search_via_tool summarised for %d variants, want 2", perVariant)
	}
	if len(detail.Deltas) == 0 {
		t.Fatal("no deltas")
	}
	if detail.Deltas[0].BaselineVariant != "base" || detail.Deltas[0].Variant != "search-first" {
		t.Errorf("delta compares %q to %q", detail.Deltas[0].BaselineVariant, detail.Deltas[0].Variant)
	}
}

func TestGetEvalNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/evals/evr-missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// An empty list is an empty array, never null: the browser maps over it.
func TestListEvalsWithNoRunsIsAnEmptyArray(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var rows []evalRunRow
	body := getJSON(t, srv.URL+"/api/evals", &rows)
	if string(body) != "[]\n" {
		t.Errorf("body = %q, want an empty array", body)
	}
}

func TestGetSessionEvalLinksBothWays(t *testing.T) {
	srv, st, _ := newTestServer(t)
	seedEval(t, st, "evr-1")

	var row evalMembershipRow
	getJSON(t, srv.URL+"/api/sessions/sess-a/eval", &row)
	if row.EvalRunID != "evr-1" || row.Suite != "search" || row.Variant != "base" {
		t.Errorf("membership = %+v", row)
	}

	// An ordinary session belongs to no eval, which is a 404 rather than an
	// empty body the browser would have to tell apart from a real answer.
	resp, err := http.Get(srv.URL + "/api/sessions/sess-ordinary/eval")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a session in no eval", resp.StatusCode)
	}
}

func TestListEvalSuitesAndVariants(t *testing.T) {
	srv, _, _ := newTestServer(t)

	var suites []evalSuiteRow
	getJSON(t, srv.URL+"/api/evals/suites", &suites)
	if len(suites) == 0 {
		t.Fatal("no suites listed")
	}
	found := false
	for _, s := range suites {
		if s.Name == "search" {
			found = true
			if len(s.TaskIDs) == 0 {
				t.Error("the search suite lists no tasks")
			}
		}
	}
	if !found {
		t.Error("the search suite is not listed")
	}

	var variants []evalVariantRow
	getJSON(t, srv.URL+"/api/evals/variants", &variants)
	if len(variants) < 2 || variants[0].Name != "base" {
		t.Fatalf("variants = %+v, want base first", variants)
	}
	for _, v := range variants {
		if v.Description == "" {
			t.Errorf("variant %q has no description", v.Name)
		}
	}
}

// A member that timed out carries a status and no error string. Counting
// only the error string reported a run where every member timed out as a
// clean success with nothing failed.
func TestListEvalsCountsABadlyEndedMemberAsFailed(t *testing.T) {
	srv, st, _ := newTestServer(t)
	run := store.EvalRun{
		ID: "evr-timeouts", Suite: "search", SuiteJSON: json.RawMessage(`{}`),
		Variants: []string{"base", "search-first"}, Replicates: 1,
		Status: store.EvalStatusOK, StartedAt: time.Now().UTC(),
	}
	members := []store.EvalMember{
		{EvalRunID: "evr-timeouts", RequestID: "a", TaskID: "t", Variant: "base", Replicate: 1, Status: "timeout"},
		{EvalRunID: "evr-timeouts", RequestID: "b", TaskID: "t", Variant: "search-first", Replicate: 1, Status: "max_turns"},
		{EvalRunID: "evr-timeouts", RequestID: "c", TaskID: "t", Variant: "base", Replicate: 1, Status: "ok"},
	}
	if err := st.CreateEvalRun(context.Background(), run, members); err != nil {
		t.Fatal(err)
	}

	var rows []evalRunRow
	getJSON(t, srv.URL+"/api/evals", &rows)
	if rows[0].Failed != 2 {
		t.Errorf("failed = %d, want 2 — a timeout and a max_turns are not clean finishes", rows[0].Failed)
	}
	if rows[0].Finished != 3 {
		t.Errorf("finished = %d, want 3 — they did reach a terminal state", rows[0].Finished)
	}
}

// A running run reports what has finished so far rather than waiting.
func TestListEvalsCountsAPartlyFinishedRun(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	run := store.EvalRun{
		ID: "evr-live", Suite: "search", SuiteJSON: json.RawMessage(`{}`),
		Variants: []string{"base", "search-first"}, Replicates: 1,
		Status: store.EvalStatusRunning, StartedAt: time.Now().UTC(),
	}
	members := []store.EvalMember{
		{EvalRunID: "evr-live", RequestID: "eval-a", TaskID: "t", Variant: "base", Replicate: 1, Status: "ok", CostUSD: 0.01},
		{EvalRunID: "evr-live", RequestID: "eval-b", TaskID: "t", Variant: "search-first", Replicate: 1, Status: "pending"},
		{EvalRunID: "evr-live", RequestID: "eval-c", TaskID: "t", Variant: "base", Replicate: 1, Status: "failed", Error: "timeout"},
	}
	if err := st.CreateEvalRun(ctx, run, members); err != nil {
		t.Fatal(err)
	}

	var rows []evalRunRow
	getJSON(t, srv.URL+"/api/evals", &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].Total != 3 || rows[0].Finished != 2 || rows[0].Failed != 1 {
		t.Errorf("counts = total %d finished %d failed %d, want 3/2/1",
			rows[0].Total, rows[0].Finished, rows[0].Failed)
	}
	if rows[0].Status != store.EvalStatusRunning {
		t.Errorf("status = %q", rows[0].Status)
	}
}

// A run resource takes PATCH and DELETE. POST is not one of its methods —
// cancelling is its own subresource — and must be refused rather than fall
// through to the static handler.
func TestPostOnARunResourceIsRefused(t *testing.T) {
	srv, st, _ := newTestServer(t)
	seedEval(t, st, "evr-1")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/evals/evr-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/evals/{id} = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "GET, HEAD, PATCH, DELETE" {
		t.Errorf("Allow = %q", allow)
	}
}

// newEvalServer builds a server with an eval controller and a control token,
// which newTestServer does not expose. The eval writes need both.
func newEvalServer(t *testing.T, ctrl EvalController) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st),
		Evals: ctrl, ControlToken: "tok",
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "static placeholder")
		}),
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

// fakeEvalController stands in for the orchestrator: it records what it was
// asked to start and answers cancel from a set the test controls.
type fakeEvalController struct {
	started   []evals.Spec
	startErr  error
	running   map[string]bool
	cancelled []string
}

func (f *fakeEvalController) StartEval(_ context.Context, spec evals.Spec) (string, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	f.started = append(f.started, spec)
	return "evr-new", nil
}

func (f *fakeEvalController) CancelEval(id string) error {
	if !f.running[id] {
		return evals.ErrEvalNotRunning
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeEvalController) RunningEval(id string) bool { return f.running[id] }

// postEval issues a write with the guards the surface requires: same origin,
// application/json, and the bearer token for run control.
func postEval(t *testing.T, srv *httptest.Server, method, path, token, ifMatch, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestStartEvalGoesThroughTheSeam(t *testing.T) {
	ctrl := &fakeEvalController{}
	srv, _ := newEvalServer(t, ctrl)

	resp := postEval(t, srv, http.MethodPost, "/api/evals", "tok", "",
		`{"suite":"search","variants":["base","search-first"],"replicates":2}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	if len(ctrl.started) != 1 || ctrl.started[0].Suite != "search" {
		t.Fatalf("started = %+v", ctrl.started)
	}
}

// A bad spec is a 400 carrying the validator's own message, not a 500 from
// halfway through a run.
func TestStartEvalRejectsABadSpec(t *testing.T) {
	ctrl := &fakeEvalController{}
	srv, _ := newEvalServer(t, ctrl)

	for name, body := range map[string]string{
		"no suite":      `{"variants":["base","search-first"],"replicates":1}`,
		"one variant":   `{"suite":"search","variants":["base"],"replicates":1}`,
		"unknown suite": `{"suite":"nope","variants":["base","search-first"],"replicates":1}`,
		"zero reps":     `{"suite":"search","variants":["base","search-first"],"replicates":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := postEval(t, srv, http.MethodPost, "/api/evals", "tok", "", body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	if len(ctrl.started) != 0 {
		t.Errorf("a rejected spec still reached the seam: %+v", ctrl.started)
	}
}

// Two evals interleaving means each measures a machine the other is loading.
func TestStartEvalRefusesASecondRun(t *testing.T) {
	srv, _ := newEvalServer(t, &fakeEvalController{startErr: evals.ErrEvalInFlight})

	resp := postEval(t, srv, http.MethodPost, "/api/evals", "tok", "",
		`{"suite":"search","variants":["base","search-first"],"replicates":1}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestEvalWritesRequireTheControlToken(t *testing.T) {
	srv, _ := newEvalServer(t, &fakeEvalController{})

	resp := postEval(t, srv, http.MethodPost, "/api/evals", "", "",
		`{"suite":"search","variants":["base","search-first"],"replicates":1}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", resp.StatusCode)
	}
}

// With no orchestrator wired the endpoint fails closed, the shape the missing
// publisher and the missing token already have.
func TestStartEvalWithoutAnOrchestratorFailsClosed(t *testing.T) {
	srv, _ := newEvalServer(t, nil)

	resp := postEval(t, srv, http.MethodPost, "/api/evals", "tok", "",
		`{"suite":"search","variants":["base","search-first"],"replicates":1}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestCancelEval(t *testing.T) {
	ctrl := &fakeEvalController{running: map[string]bool{"evr-live": true}}
	srv, st := newEvalServer(t, ctrl)
	ctx := context.Background()
	if err := st.CreateEvalRun(ctx, store.EvalRun{
		ID: "evr-live", Suite: "search", SuiteJSON: json.RawMessage(`{}`),
		Variants: []string{"base", "search-first"}, Replicates: 1,
		Status: store.EvalStatusRunning, StartedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}

	resp := postEval(t, srv, http.MethodPost, "/api/evals/evr-live/cancel", "tok", "", "{}")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	if len(ctrl.cancelled) != 1 {
		t.Errorf("cancelled = %v", ctrl.cancelled)
	}

	// A finished run has nothing to cancel.
	if err := st.FinishEvalRun(ctx, "evr-live", store.EvalStatusOK, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	resp2 := postEval(t, srv, http.MethodPost, "/api/evals/evr-live/cancel", "tok", "", "{}")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 on a finished run", resp2.StatusCode)
	}
}

// PATCH closes out a run stranded by a dead orchestrator, and carries
// If-Match like every other row write.
func TestPatchEvalClosesAStrandedRun(t *testing.T) {
	srv, st := newEvalServer(t, &fakeEvalController{})
	ctx := context.Background()
	if err := st.CreateEvalRun(ctx, store.EvalRun{
		ID: "evr-stranded", Suite: "search", SuiteJSON: json.RawMessage(`{}`),
		Variants: []string{"base", "search-first"}, Replicates: 1,
		Status: store.EvalStatusRunning, StartedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Without If-Match the write cannot be proven fresh.
	missing := postEval(t, srv, http.MethodPatch, "/api/evals/evr-stranded", "", "", `{"status":"cancelled"}`)
	missing.Body.Close()
	if missing.StatusCode != http.StatusPreconditionRequired {
		t.Errorf("status = %d, want 428 without If-Match", missing.StatusCode)
	}

	stale := postEval(t, srv, http.MethodPatch, "/api/evals/evr-stranded", "", "99", `{"status":"cancelled"}`)
	stale.Body.Close()
	if stale.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 on a stale version", stale.StatusCode)
	}

	ok := postEval(t, srv, http.MethodPatch, "/api/evals/evr-stranded", "", "1", `{"status":"cancelled"}`)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(ok.Body)
		t.Fatalf("status = %d: %s", ok.StatusCode, b)
	}
	run, err := st.GetEvalRun(ctx, "evr-stranded")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.EvalStatusCancelled || run.FinishedAt == nil {
		t.Errorf("run = %+v", run)
	}
}

// A run this process is actually executing must be cancelled, not closed out
// underneath itself.
func TestPatchEvalRefusesARunStillInFlight(t *testing.T) {
	srv, st := newEvalServer(t, &fakeEvalController{running: map[string]bool{"evr-live": true}})
	if err := st.CreateEvalRun(context.Background(), store.EvalRun{
		ID: "evr-live", Suite: "search", SuiteJSON: json.RawMessage(`{}`),
		Variants: []string{"base", "search-first"}, Replicates: 1,
		Status: store.EvalStatusRunning, StartedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}

	resp := postEval(t, srv, http.MethodPatch, "/api/evals/evr-live", "", "1", `{"status":"cancelled"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestDeleteEvalRefusesARunningRunAndRemovesAFinishedOne(t *testing.T) {
	srv, st := newEvalServer(t, &fakeEvalController{})
	ctx := context.Background()
	seedEval(t, st, "evr-done")
	if err := st.CreateEvalRun(ctx, store.EvalRun{
		ID: "evr-live", Suite: "search", SuiteJSON: json.RawMessage(`{}`),
		Variants: []string{"base", "search-first"}, Replicates: 1,
		Status: store.EvalStatusRunning, StartedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}

	live := postEval(t, srv, http.MethodDelete, "/api/evals/evr-live", "", "1", "")
	live.Body.Close()
	if live.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 deleting a running run", live.StatusCode)
	}

	done := postEval(t, srv, http.MethodDelete, "/api/evals/evr-done", "", "1", "")
	defer done.Body.Close()
	if done.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(done.Body)
		t.Fatalf("status = %d: %s", done.StatusCode, b)
	}
	if _, err := st.GetEvalRun(ctx, "evr-done"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("run survived the delete: %v", err)
	}
}

// The suites and variants collections sit under /api/evals/ and are reads;
// they must not be mistaken for a run id and opened to writes.
func TestEvalSubcollectionsStayReadOnly(t *testing.T) {
	srv, _ := newEvalServer(t, &fakeEvalController{})
	for _, path := range []string{"/api/evals/suites", "/api/evals/variants"} {
		resp := postEval(t, srv, http.MethodDelete, path, "", "1", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("DELETE %s = %d, want 405", path, resp.StatusCode)
		}
	}
}
