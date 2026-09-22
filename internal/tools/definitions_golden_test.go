package tools_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// TestToolArrayGolden pins the exact serialised tool array of each provider
// against its own committed golden file. There are two frozen request head
// shapes now — DeepSeek's twenty tools, and the vision-capable fourteen
// without the six vision tools (docs/KIMI-INTEGRATION.md decisions 5 and 6)
// — and both need the byte-stability guard the single array used to have:
// the head of every request is frozen and shared (docs/DESIGN.md §3.2,
// docs/CACHE.md), and the prompt cache depends on identical bytes. Gemini
// joined the vision-capable shape in Phase 7 (docs/GEMINI-INTEGRATION.md
// §5.7, §7): its array is byte-identical to Kimi's — both drop the same six
// tools — so its case below reads the same golden file rather than a
// duplicate; TestDefinitionsForProviderGeminiMatchesKimi pins the equality
// directly.
//
// One test parameterised over the provider shapes, with a golden file per
// shape. The files are the choice, not the test: a failing assertion names
// the case whose head moved (each case carries its own golden path, so the
// failure text and the diff both say which one), each file can be
// regenerated independently, and the table-driven body keeps the assertions
// from drifting apart. A single file for both arrays would save nothing and
// blur which provider a change belongs to.
//
// Neither file is a claim that the array never changes — it is a claim that
// it never changes by accident. Regenerating one is a deliberate act with a
// cost attached: the head is the shared prompt-cache prefix, so a moved byte
// invalidates the cache for every session on that provider. Both goldens were
// last moved on purpose to tell Bash's `timeout` argument that a default and a
// ceiling exist and that a request above the ceiling is clamped rather than
// refused — models were routinely asking for thirty and sixty minutes, being
// cut off at the ceiling, and reading the bare "command timed out" as a hung
// command. Bash is in both arrays, so both files moved together.
func TestToolArrayGolden(t *testing.T) {
	cases := []struct {
		name   string
		model  string // a model the provider serves, exercising the resolution path
		golden string
	}{
		{"deepseek", "deepseek-v4-pro", "tools_deepseek.golden.json"},
		{"kimi", "kimi-k3", "tools_kimi.golden.json"},
		{"gemini", "gemini-3.7-flash", "tools_kimi.golden.json"},
		// deepseek-flash is the model DefinitionsFor resolves per model
		// rather than per provider for (provider.SeesImages,
		// docs/DEEPSEEK-VISION.md). Its capability is true, so — like Kimi
		// and Gemini — it resolves to definitionsVisionCapable and reads
		// Kimi's golden file rather than a third copy of the same bytes.
		{"deepseek-flash", "deepseek-flash", "tools_kimi.golden.json"},
		// Claude reads images natively like Kimi and Gemini, but also drops
		// WebFetch — Anthropic's own server-side web_search and web_fetch
		// tools ride the request instead (internal/anthropic's intent
		// renderer, docs/ANTHROPIC-INTEGRATION.md) — so its array is one
		// tool shorter than Kimi's and gets its own golden file.
		{"claude", "claude-sonnet-5", "tools_claude.golden.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tools.DefinitionsFor(tc.model))
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatalf("ReadFile golden: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("%s tool array differs from %s:\ngot:  %s\nwant: %s",
					tc.name, tc.golden, got, want)
			}
		})
	}
}

// TestDefinitionsForProviderShape pins the relationship between the two
// arrays directly: Kimi's is DeepSeek's twenty minus exactly the six
// vision tools, in the same order, with the surviving tools byte-identical. The
// golden files pin the bytes; this pins the subtraction.
func TestDefinitionsForProviderShape(t *testing.T) {
	deepseek := tools.DefinitionsFor("deepseek-v4-pro")
	kimi := tools.DefinitionsFor("kimi-k3")
	if len(deepseek) != 22 {
		t.Fatalf("DeepSeek array has %d tools, want 22", len(deepseek))
	}
	if len(kimi) != 16 {
		t.Fatalf("Kimi array has %d tools, want 16", len(kimi))
	}

	// Walk both arrays with two pointers; DeepSeek's skips the six vision
	// tools, Kimi's does not. Every surviving pair must be byte-identical.
	dj, _ := json.Marshal(deepseek)
	var ds []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(dj, &ds); err != nil {
		t.Fatal(err)
	}
	dropped := []string{"Screenshot", "Glance", "Ground", "Detect", "Transcribe", "Crop"}
	var j int
	for i, d := range ds {
		isDropped := false
		for _, name := range dropped {
			if d.Function.Name == name {
				isDropped = true
				break
			}
		}
		if isDropped {
			continue
		}
		if j >= len(kimi) {
			t.Fatalf("Kimi array ended early at DeepSeek index %d (%s)", i, d.Function.Name)
		}
		kj, _ := json.Marshal(kimi[j])
		di, _ := json.Marshal(deepseek[i])
		if string(kj) != string(di) {
			t.Fatalf("Kimi tool %s (index %d) differs from the DeepSeek tool of the same name:\ngot:  %s\nwant: %s",
				kimi[j].Function.Name, j, kj, di)
		}
		j++
	}
	if j != len(kimi) {
		t.Fatalf("Kimi array has %d entries the DeepSeek array does not account for", len(kimi)-j)
	}
}

// TestDefinitionsForProviderGeminiMatchesKimi pins that Gemini's tool array
// is not merely golden-equal to Kimi's by coincidence of two committed
// files agreeing, but the same relationship: both kimi-k3 and
// gemini-3.7-flash read images natively (provider.SeesImages), so both drop
// the identical six DeepSeek-only vision tools
// (docs/GEMINI-INTEGRATION.md §5.7), and DefinitionsFor resolves both to the
// one definitionsVisionCapable array rather than two copies of it. This is a
// deliberate sameness, not a coincidence to be surprised by later — if
// Gemini and Kimi ever need to diverge (a vision tool one of them should
// keep and the other should not), that is the day this test stops passing
// and a second array is warranted.
func TestDefinitionsForProviderGeminiMatchesKimi(t *testing.T) {
	kimi := tools.DefinitionsFor("kimi-k3")
	gemini := tools.DefinitionsFor("gemini-3.7-flash")
	kj, err := json.Marshal(kimi)
	if err != nil {
		t.Fatalf("Marshal(kimi): %v", err)
	}
	gj, err := json.Marshal(gemini)
	if err != nil {
		t.Fatalf("Marshal(gemini): %v", err)
	}
	if string(kj) != string(gj) {
		t.Fatalf("gemini tool array differs from kimi's:\ngemini: %s\nkimi:   %s", gj, kj)
	}
}
