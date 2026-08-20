package mcpclient

import (
	"strings"
	"testing"
)

func TestQualifyToolNamePlain(t *testing.T) {
	got := QualifyToolName("blender", "get_objects_summary")
	want := "mcp__blender__get_objects_summary"
	if got != want {
		t.Fatalf("QualifyToolName() = %q, want %q", got, want)
	}
}

func TestQualifyToolNameAlreadyLegal(t *testing.T) {
	// A name already inside [A-Za-z0-9_-] must pass through unchanged
	// apart from gaining the prefix — sanitising must not touch legal
	// characters.
	got := QualifyToolName("srv", "Do-The-Thing_123")
	want := "mcp__srv__Do-The-Thing_123"
	if got != want {
		t.Fatalf("QualifyToolName() = %q, want %q", got, want)
	}
}

func TestQualifyToolNameSanitisesIllegalCharacters(t *testing.T) {
	got := QualifyToolName("srv", "search docs/v2.0 (beta)")
	want := "mcp__srv__search_docs_v2_0__beta_"
	if got != want {
		t.Fatalf("QualifyToolName() = %q, want %q", got, want)
	}
}

func TestQualifyToolNameOverLongIsTruncatedWithHashSuffix(t *testing.T) {
	tool := strings.Repeat("x", 100)
	got := QualifyToolName("srv", tool)

	if len(got) != maxToolNameLen {
		t.Fatalf("len(QualifyToolName()) = %d, want %d (got %q)", len(got), maxToolNameLen, got)
	}
	wantPrefix := "mcp__srv__"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("QualifyToolName() = %q, want prefix %q", got, wantPrefix)
	}
	wantSuffix := "_" + hashSuffix(tool)
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("QualifyToolName() = %q, want suffix %q", got, wantSuffix)
	}

	// Deterministic: the same overlong name probed again produces the
	// identical qualified name.
	again := QualifyToolName("srv", tool)
	if again != got {
		t.Fatalf("QualifyToolName() is not deterministic: %q != %q", again, got)
	}
}

func TestQualifyToolNameOverLongStaysWithinLimitWithLongServerName(t *testing.T) {
	server := strings.Repeat("s", 32) // the longest a server name may be
	tool := strings.Repeat("y", 80)
	got := QualifyToolName(server, tool)
	if len(got) > maxToolNameLen {
		t.Fatalf("len(QualifyToolName()) = %d, want <= %d (got %q)", len(got), maxToolNameLen, got)
	}
}

func TestQualifyToolNamesResolvesCollisionsWithinOneServer(t *testing.T) {
	// "get objects" and "get.objects" both sanitise to "get_objects".
	names := []string{"get objects", "get.objects", "get_objects"}
	got := QualifyToolNames("srv", names)

	if got[0] != "mcp__srv__get_objects" {
		t.Fatalf("first name = %q, want unsuffixed", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[0] {
			t.Fatalf("name %d (%q) collides with name 0 (%q)", i, got[i], got[0])
		}
	}
	// Every entry must still be unique.
	seen := map[string]bool{}
	for i, n := range got {
		if seen[n] {
			t.Fatalf("duplicate qualified name %q at index %d: %v", n, i, got)
		}
		seen[n] = true
	}

	// Deterministic given the same input names, regardless of how many
	// collisions preceded a given entry.
	again := QualifyToolNames("srv", names)
	for i := range got {
		if got[i] != again[i] {
			t.Fatalf("QualifyToolNames is not deterministic at index %d: %q != %q", i, got[i], again[i])
		}
	}
}

func TestQualifyToolNamesNoCollisionLeavesNamesAlone(t *testing.T) {
	names := []string{"alpha", "beta", "gamma"}
	got := QualifyToolNames("srv", names)
	want := []string{"mcp__srv__alpha", "mcp__srv__beta", "mcp__srv__gamma"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("QualifyToolNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
