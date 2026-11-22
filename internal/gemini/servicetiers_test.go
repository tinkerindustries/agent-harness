package gemini

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

func TestServiceTiersFor(t *testing.T) {
	for _, m := range []string{
		"gemini-4-neon", "gemini-4-hydrogen", "gemini-4-carbon", "gemini-4-argon",
		"gemini-3.8-flash", "gemini-3.6-flash", "gemini-3.5-flash-lite",
	} {
		if got := ServiceTiersFor(m); !slices.Equal(got, []string{ServiceTierFlex}) {
			t.Errorf("ServiceTiersFor(%s) = %v, want [flex]", m, got)
		}
	}
	for _, m := range []string{"gemini-3.7-flash", "gemini-3.5-flash", "not-a-model"} {
		if got := ServiceTiersFor(m); got != nil {
			t.Errorf("ServiceTiersFor(%s) = %v, want none", m, got)
		}
	}
}

func TestServiceTiersForReturnsACopy(t *testing.T) {
	got := ServiceTiersFor("gemini-4-neon")
	got[0] = "changed"
	if again := ServiceTiersFor("gemini-4-neon"); again[0] != ServiceTierFlex {
		t.Errorf("a caller changed the table: %v", again)
	}
}

func TestServiceTierSupported(t *testing.T) {
	for _, tc := range []struct {
		model, tier string
		want        bool
	}{
		{"gemini-4-neon", ServiceTierFlex, true},
		{"gemini-3.7-flash", ServiceTierFlex, false},
		{"not-a-model", ServiceTierFlex, false},
		{"gemini-3.7-flash", ServiceTierStandard, true},
		{"gemini-3.7-flash", "", true},
		{"gemini-4-neon", "priority", false},
	} {
		if got := ServiceTierSupported(tc.model, tc.tier); got != tc.want {
			t.Errorf("ServiceTierSupported(%s, %q) = %v, want %v", tc.model, tc.tier, got, tc.want)
		}
	}
}

func TestRequestFromIntentServiceTier(t *testing.T) {
	user := wire.UserItem("hi")
	for _, tc := range []struct {
		tier     string
		wantSent bool
	}{
		{ServiceTierFlex, true},
		{ServiceTierStandard, false},
		{"", false},
	} {
		body, err := json.Marshal(requestFromIntent(wire.ChatIntent{Model: "gemini-4-neon", Items: []wire.Item{user}, ServiceTier: tc.tier}))
		if err != nil {
			t.Fatal(err)
		}
		has := strings.Contains(string(body), `"service_tier"`)
		if has != tc.wantSent {
			t.Errorf("tier %q: body has service_tier = %v, want %v: %s", tc.tier, has, tc.wantSent, body)
		}
		if tc.wantSent && !strings.HasSuffix(string(body), `"service_tier":"flex"}`) {
			t.Errorf("service_tier is not the last field: %s", body)
		}
	}
}
