package store

import (
	"strings"
	"testing"
)

// TestBackgroundFinishedMessageNamesTheJobTools pins the wording of the notice
// the model reads when a background shell exits. It is the only place a model
// learns how to read the shell's output, so a rename that misses this string
// would tell it to call a tool its array does not carry.
func TestBackgroundFinishedMessageNamesTheJobTools(t *testing.T) {
	got := BackgroundFinishedPayload{
		BashID: "bash_1", Command: "scripts/build.sh", ExitCode: 0,
	}.Message()
	want := `Background shell bash_1 (scripts/build.sh) finished with exit code 0. ` +
		`Its output is still readable with job_output using job_id "bash_1".`
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	for _, old := range []string{"BashOutput", "KillBash", "bash_id"} {
		if strings.Contains(got, old) {
			t.Errorf("message still names %q: %q", old, got)
		}
	}
}

func TestBackgroundFinishedMessageReportsKilledAndWedged(t *testing.T) {
	killed := BackgroundFinishedPayload{BashID: "bash_2", Killed: true}.Message()
	if !strings.Contains(killed, "was killed before it finished.") {
		t.Errorf("killed message = %q", killed)
	}
	wedged := BackgroundFinishedPayload{BashID: "bash_3", Wedged: true}.Message()
	if !strings.Contains(wedged, "left a process holding its output open") {
		t.Errorf("wedged message = %q", wedged)
	}
}

// TestRetiredGuardDecisionStaysReadable pins that a state directory written
// by an earlier autonomy guard, whose log holds guard_decision rows, still
// loads and can be filtered by that kind. Nothing writes the kind now.
func TestRetiredGuardDecisionStaysReadable(t *testing.T) {
	if !ValidEventKind(string(KindGuardDecision)) {
		t.Fatal("guard_decision is not in EventKinds")
	}
	s := openTestStore(t)
	ctx := t.Context()
	mustCreateSession(t, s, "sess-1")
	old := map[string]any{"tool_call_id": "call_1", "mode": "shadow", "verdict": "allow"}
	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{{Kind: KindGuardDecision, Payload: old}}); err != nil {
		t.Fatal(err)
	}
	events, err := s.GetEvents(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		found = found || e.Kind == KindGuardDecision
	}
	if !found {
		t.Fatal("the guard_decision row did not read back")
	}
}
