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
