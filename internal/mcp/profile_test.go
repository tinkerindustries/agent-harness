package mcp

import "testing"

// TestResolveProfile pins resolveProfile's mapping against docs/MODELS.md's
// per-role table. The flash row is the one with hardcoded values — the model
// threaded in from config and the effort the table's `Task` subagent row
// names (max) — and it flows into queue.Request untouched, so a regression
// here would otherwise pass silently downstream.
func TestResolveProfile(t *testing.T) {
	cases := []struct {
		name       string
		profile    string
		wantModel  string
		wantEffort string
		wantErr    bool
	}{
		{name: "empty defers to the pool default", profile: "", wantModel: "", wantEffort: ""},
		{name: "pro defers to the pool default", profile: "pro", wantModel: "", wantEffort: ""},
		{name: "flash names the flash model at max effort", profile: "flash", wantModel: "deepseek-v4-flash", wantEffort: "max"},
		{name: "unknown profile is an error", profile: "ultra", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, effort, err := resolveProfile(tc.profile, "deepseek-v4-flash")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for an unrecognised profile")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveProfile(%q): %v", tc.profile, err)
			}
			if model != tc.wantModel || effort != tc.wantEffort {
				t.Fatalf("resolveProfile(%q) = (%q, %q), want (%q, %q)", tc.profile, model, effort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}
