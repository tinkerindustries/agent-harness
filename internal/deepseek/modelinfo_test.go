package deepseek

import (
	"slices"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// The advertised set is what the API documents as its possible values, and
// nothing else: low, high, max (third_party/deepseek-docs/api/
// create-chat-completion.md).
func TestEffortsForAdvertisesTheDocumentedSet(t *testing.T) {
	got := EffortsFor("deepseek-v4-flash-vision-exp")
	want := []string{wire.EffortLow, wire.EffortHigh, wire.EffortMax}
	if !slices.Equal(got, want) {
		t.Errorf("EffortsFor = %v, want %v", got, want)
	}
	if EffortsFor("gemini-3.7-flash") != nil {
		t.Error("EffortsFor answered for another provider's model")
	}
	// The caller may sort or truncate its copy without editing the table.
	got[0] = "edited"
	if EffortsFor("deepseek-v4-flash-vision-exp")[0] != wire.EffortLow {
		t.Error("EffortsFor handed out the table itself")
	}
}

// Acceptance is wider than the advertisement, and deliberately: DeepSeek
// maps "medium" and "xhigh" onto high rather than rejecting them, and
// "medium" is one of the four levels a client speaking Google's vocabulary
// over docs/STDIO-PROTOCOL.md may already offer. "minimal" is the fourth of
// those and is documented nowhere here, so it is refused before a run
// starts rather than 400ing in the middle of one.
func TestEffortSupported(t *testing.T) {
	const model = "deepseek-v4-flash-vision-exp"
	for _, effort := range []string{wire.EffortLow, wire.EffortHigh, wire.EffortMax, "medium", "xhigh"} {
		if !EffortSupported(model, effort) {
			t.Errorf("EffortSupported(%s, %q) = false, want true", model, effort)
		}
	}
	if EffortSupported(model, "minimal") {
		t.Errorf("EffortSupported(%s, \"minimal\") = true; DeepSeek documents no such effort", model)
	}
	// A model no table knows accepts anything, so a table nobody updated is
	// never what stops a new model working — the same rule
	// internal/gemini.LevelSupported follows.
	if !EffortSupported("deepseek-v5-something", "minimal") {
		t.Error("an unknown model was refused on the strength of this table")
	}
}

// The two descriptive fields answer for the models this package serves and
// stay quiet about everything else: a zero context window is omitted from
// the handshake rather than published as a guess, and an empty display name
// makes a client fall back to the model id.
func TestDescriptiveTables(t *testing.T) {
	if ContextWindowTokens("deepseek-v4-flash-vision-exp") <= 0 {
		t.Error("the vision model has no context window figure")
	}
	if DisplayName("deepseek-v4-flash-vision-exp") == "" {
		t.Error("the vision model has no display name")
	}
	if got := ContextWindowTokens("gemini-3.7-flash"); got != 0 {
		t.Errorf("ContextWindowTokens for another provider's model = %d, want 0", got)
	}
	if got := DisplayName("gemini-3.7-flash"); got != "" {
		t.Errorf("DisplayName for another provider's model = %q, want empty", got)
	}
}

// Every model internal/provider routes to DeepSeek has an entry in all three
// tables. A model added to the routing table and forgotten here would
// advertise no efforts and no context window, which a client reads as "this
// process constrains nothing" — a silent wrong answer rather than a loud one.
func TestEveryDeepSeekModelHasEntries(t *testing.T) {
	for _, m := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		if EffortsFor(m) == nil {
			t.Errorf("%s has no advertised efforts", m)
		}
		if ContextWindowTokens(m) <= 0 {
			t.Errorf("%s has no context window figure", m)
		}
		if DisplayName(m) == "" {
			t.Errorf("%s has no display name", m)
		}
	}
}
