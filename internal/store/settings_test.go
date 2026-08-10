package store

import (
	"context"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, ok, err := s.Setting(ctx, "deepseek.api_key"); err != nil || ok {
		t.Fatalf("Setting on empty table: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := s.SetSetting(ctx, "deepseek.api_key", "sk-abc"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	value, ok, err := s.Setting(ctx, "deepseek.api_key")
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if !ok || value != "sk-abc" {
		t.Fatalf("Setting = %q ok=%v, want %q ok=true", value, ok, "sk-abc")
	}

	// SetSetting upserts: a second write replaces the value, it does not
	// fail on the primary key.
	if err := s.SetSetting(ctx, "deepseek.api_key", "sk-def"); err != nil {
		t.Fatalf("SetSetting upsert: %v", err)
	}
	value, _, err = s.Setting(ctx, "deepseek.api_key")
	if err != nil {
		t.Fatal(err)
	}
	if value != "sk-def" {
		t.Fatalf("Setting after upsert = %q, want %q", value, "sk-def")
	}

	if err := s.SetSetting(ctx, "google.api_key", "AI-xyz"); err != nil {
		t.Fatalf("SetSetting second key: %v", err)
	}

	all, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if len(all) != 2 || all["deepseek.api_key"] != "sk-def" || all["google.api_key"] != "AI-xyz" {
		t.Fatalf("Settings() = %v, want both keys", all)
	}

	if err := s.DeleteSetting(ctx, "deepseek.api_key"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if _, ok, err := s.Setting(ctx, "deepseek.api_key"); err != nil || ok {
		t.Fatalf("Setting after delete: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	// Deleting a key with no row is not an error.
	if err := s.DeleteSetting(ctx, "deepseek.api_key"); err != nil {
		t.Fatalf("DeleteSetting twice: %v", err)
	}
}
