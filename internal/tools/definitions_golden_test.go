package tools_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// TestToolArrayGolden pins the exact serialised tool array of each provider
// against its own committed golden file. There are two frozen request heads
// now — DeepSeek's nineteen tools, Kimi's fourteen without the five vision
// tools (docs/KIMI-INTEGRATION.md decisions 5 and 6) — and both need the
// byte-stability guard the single array used to have: the head of every
// request is frozen and shared (docs/DESIGN.md §3.2, docs/CACHE.md), and the
// prompt cache depends on identical bytes.
//
// One test parameterised over the two providers, with a golden file per
// provider. The files are the choice, not the test: a failing assertion
// names the provider whose head moved (each case carries its own golden
// path, so the failure text and the diff both say which one), each file can
// be regenerated independently, and the table-driven body keeps the two
// assertions from drifting apart. A single file for both arrays would save
// nothing and blur which provider a change belongs to.
//
// Neither file is a claim that the array never changes — it is a claim that
// it never changes by accident. Regenerating one is a deliberate act with a
// cost attached: the head is the shared prompt-cache prefix, so a moved byte
// invalidates the cache for every session on that provider and the change is
// at least a minor release (RELEASE.md). The DeepSeek golden was last moved
// on purpose to replace ReviewScreenshot and AskVision with Glance, Ground,
// Detect, and Crop, ported from agent-vision-toolkit
// (docs/VISION-TOOLKIT.md); the Kimi array drops all five vision tools and
// so was untouched by that change.
func TestToolArrayGolden(t *testing.T) {
	cases := []struct {
		name   string
		model  string // a model the provider serves, exercising the resolution path
		golden string
	}{
		{"deepseek", "deepseek-v4-pro", "tools_deepseek.golden.json"},
		{"kimi", "kimi-k3", "tools_kimi.golden.json"},
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
// arrays directly: Kimi's is DeepSeek's nineteen minus exactly the five
// vision tools, in the same order, with the surviving tools byte-identical. The
// golden files pin the bytes; this pins the subtraction.
func TestDefinitionsForProviderShape(t *testing.T) {
	deepseek := tools.DefinitionsFor("deepseek-v4-pro")
	kimi := tools.DefinitionsFor("kimi-k3")
	if len(deepseek) != 19 {
		t.Fatalf("DeepSeek array has %d tools, want 19", len(deepseek))
	}
	if len(kimi) != 14 {
		t.Fatalf("Kimi array has %d tools, want 14", len(kimi))
	}

	// Walk both arrays with two pointers; DeepSeek's skips the three vision
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
	dropped := []string{"Screenshot", "Glance", "Ground", "Detect", "Crop"}
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
