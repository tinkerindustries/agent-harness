package store

import (
	"database/sql"
	"testing"
)

func TestSessionGuardSettingRoundTripsAndCanBeReplaced(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	base := Session{ID: "sess_g", Model: "m", Effort: "high", Workspace: "/w", PermissionMode: "full"}

	want := GuardSetting{ClassifyURL: "http://gateway.invalid/classify", Scenario: "repo", Engine: "deepseek"}
	withGuard := base
	withGuard.Guard = want
	if err := s.CreateSession(ctx, withGuard); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, "sess_g")
	if err != nil {
		t.Fatal(err)
	}
	if got.Guard != want {
		t.Fatalf("guard = %+v", got.Guard)
	}

	if err := s.UpdateSessionGuard(ctx, "sess_g", GuardSetting{}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSession(ctx, "sess_g"); got.Guard != (GuardSetting{}) || got.Guard.Active() {
		t.Fatalf("guard after update = %+v", got.Guard)
	}
	if err := s.UpdateSessionGuard(ctx, "missing", want); err != ErrNotFound {
		t.Fatalf("update of a missing session = %v", err)
	}

	plain := base
	plain.ID = "sess_plain"
	if err := s.CreateSession(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSession(ctx, "sess_plain"); got.Guard != (GuardSetting{}) {
		t.Fatalf("a session with no guard reads back %+v", got.Guard)
	}
}

// TestRowFromTheOldGuardReadsAsOff pins that a state directory written when the
// guard was configured by a mode, a stage 1 endpoint and a policy hash still
// opens, and its session reads back with no guard rather than a half-set one.
func TestRowFromTheOldGuardReadsAsOff(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	if err := s.CreateSession(ctx, Session{ID: "sess_old", Model: "m", Effort: "high", Workspace: "/w", PermissionMode: "full"}); err != nil {
		t.Fatal(err)
	}
	old := `{"mode":"shadow","stage1_endpoint":"http://stage1.invalid","policy_hash":"abc123"}`
	if err := s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sessions SET guard = ? WHERE id = ?`, old, "sess_old")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, "sess_old")
	if err != nil {
		t.Fatal(err)
	}
	if got.Guard.Active() {
		t.Fatalf("an old guard row reads as active: %+v", got.Guard)
	}
}
