package store

import "testing"

func TestSessionGuardSettingRoundTripsAndCanBeReplaced(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	base := Session{ID: "sess_g", Model: "m", Effort: "high", Workspace: "/w", PermissionMode: "full"}

	want := GuardSetting{Mode: GuardModeShadow, Stage1Endpoint: "http://stage1.invalid", PolicyHash: "abc123"}
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

	if err := s.UpdateSessionGuard(ctx, "sess_g", GuardSetting{Mode: GuardModeOff}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSession(ctx, "sess_g"); got.Guard != (GuardSetting{Mode: GuardModeOff}) || got.Guard.Active() {
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
