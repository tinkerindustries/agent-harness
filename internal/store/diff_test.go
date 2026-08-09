package store

import "testing"

func TestComputeDiffUnchangedLinesAreContext(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nx\nc\n"
	got := ComputeDiff(old, new)

	want := []DiffLine{
		{Kind: DiffContext, Text: "a", OldLine: 1, NewLine: 1},
		{Kind: DiffRemove, Text: "b", OldLine: 2},
		{Kind: DiffAdd, Text: "x", NewLine: 2},
		{Kind: DiffContext, Text: "c", OldLine: 3, NewLine: 3},
		{Kind: DiffContext, Text: "", OldLine: 4, NewLine: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComputeDiffNoCommonLines(t *testing.T) {
	got := ComputeDiff("func f() {}", "func g() {}")
	if len(got) != 2 {
		t.Fatalf("expected 2 lines (one remove, one add), got %+v", got)
	}
	if got[0].Kind != DiffRemove || got[0].Text != "func f() {}" {
		t.Fatalf("unexpected first line: %+v", got[0])
	}
	if got[1].Kind != DiffAdd || got[1].Text != "func g() {}" {
		t.Fatalf("unexpected second line: %+v", got[1])
	}
}

func TestComputeDiffEmptyOld(t *testing.T) {
	got := ComputeDiff("", "new content\n")
	for _, l := range got {
		if l.Kind == DiffRemove {
			t.Fatalf("did not expect a remove line against empty old text: %+v", got)
		}
	}
}

func TestComputeDiffFallsBackPastLineCeiling(t *testing.T) {
	big := make([]byte, 0)
	for i := 0; i < maxLCSLines+10; i++ {
		big = append(big, "x\n"...)
	}
	got := ComputeDiff(string(big), "y\n")
	// The coarse fallback removes every old line and adds every new line,
	// never aligning any of them as context.
	for _, l := range got {
		if l.Kind == DiffContext {
			t.Fatalf("did not expect context lines from the coarse fallback: found one in %d lines", len(got))
		}
	}
}
