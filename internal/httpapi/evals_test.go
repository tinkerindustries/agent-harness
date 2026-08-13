package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
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

// Writes are not part of this phase, so the surface must refuse them rather
// than fall through to the static handler.
func TestEvalPathsAreReadOnly(t *testing.T) {
	srv, st, _ := newTestServer(t)
	seedEval(t, st, "evr-1")

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		req, err := http.NewRequest(method, srv.URL+"/api/evals/evr-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/evals/{id} = %d, want 405", method, resp.StatusCode)
		}
	}
}
