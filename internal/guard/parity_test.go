package guard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The parity fixtures are the output of the Python the rules are ported from.
// The synthetic set is committed. The full set holds private commands, so a
// failure on it names the case id, the file and the field and nothing else.

const parityEnv = "GUARD_PARITY_DIR"

type parityRow struct {
	ID               string                     `json:"id"`
	Command          string                     `json:"command"`
	Branch           *string                    `json:"branch"`
	ScriptsRun       []paritySript              `json:"scripts_run"`
	WorkingDirectory string                     `json:"working_directory"`
	Repositories     []string                   `json:"repositories"`
	Expected         any                        `json:"expected"`
	Files            map[string]json.RawMessage `json:"files"`
}

type paritySript struct {
	Path    string  `json:"path"`
	Origin  string  `json:"origin"`
	Content *string `json:"content"`
}

func readParityRows(t *testing.T, dir, name string) []parityRow {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, name+".jsonl"))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer f.Close()
	var rows []parityRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var row parityRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("%s line %d: %v", name, len(rows)+1, err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return rows
}

// normalise turns a Go value into the generic JSON form the fixtures decode to.
func normalise(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func stringList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

type snapshotFS struct {
	files   map[string]json.RawMessage
	missing map[string]bool
}

func (s *snapshotFS) entry(p string) (kind string, text string) {
	raw, ok := s.files[p]
	if !ok {
		s.missing[p] = true
		return "null", ""
	}
	var str *string
	if json.Unmarshal(raw, &str) == nil {
		if str == nil {
			return "null", ""
		}
		return "file", *str
	}
	var flags map[string]bool
	if json.Unmarshal(raw, &flags) == nil {
		if flags["directory"] {
			return "directory", ""
		}
		if flags["unreadable"] {
			return "unreadable", ""
		}
	}
	return "null", ""
}

func (s *snapshotFS) IsDir(p string) bool { k, _ := s.entry(p); return k == "directory" }
func (s *snapshotFS) IsFile(p string) bool {
	k, _ := s.entry(p)
	return k == "file" || k == "unreadable"
}
func (s *snapshotFS) ReadText(p string) (string, error) {
	k, text := s.entry(p)
	if k != "file" {
		return "", errNotUTF8
	}
	return text, nil
}

func runParity(t *testing.T, dir string) {
	counts := map[string]int{}
	check := func(file string, rows []parityRow, got func(parityRow) any) {
		for _, row := range rows {
			counts[file]++
			if m, ok := row.Expected.(map[string]any); ok && m["error"] != nil {
				t.Errorf("case %s in %s: fixture holds an error object, which the port does not model (field expected)", row.ID, file)
				continue
			}
			actual := got(row)
			if !reflect.DeepEqual(row.Expected, actual) {
				t.Errorf("case %s in %s: field expected differs from the port's result", row.ID, file)
			}
		}
	}

	check("check", readParityRows(t, dir, "check"), func(r parityRow) any {
		branch := ""
		if r.Branch != nil {
			branch = *r.Branch
		}
		return normalise(t, stringList(NewRules().CheckRules(r.Command, branch)))
	})
	check("executed_scripts", readParityRows(t, dir, "executed_scripts"), func(r parityRow) any {
		return normalise(t, stringList(ExecutedScripts(r.Command)))
	})
	check("escalation_reasons", readParityRows(t, dir, "escalation_reasons"), func(r parityRow) any {
		var scripts []ScriptEntry
		for _, s := range r.ScriptsRun {
			scripts = append(scripts, ScriptEntry{Path: s.Path, Origin: s.Origin, Content: s.Content})
		}
		return normalise(t, stringList(NewScriptDetector().EscalationReasons(r.Command, scripts)))
	})
	check("package_scripts", readParityRows(t, dir, "package_scripts"), func(r parityRow) any {
		fsys := &snapshotFS{files: r.Files, missing: map[string]bool{}}
		entries := ResolvePackageScripts(fsys, r.Command, r.WorkingDirectory, r.Repositories)
		if len(fsys.missing) > 0 {
			t.Errorf("case %s in package_scripts: the port read a path the snapshot lacks (field files)", r.ID)
		}
		return normalise(t, entries)
	})

	var names []string
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("%s: %d cases", name, counts[name])
	}
	for _, name := range names {
		if counts[name] == 0 {
			t.Errorf("%s: no cases read", name)
		}
	}
	_ = fmt.Sprint
}

func TestParitySynthetic(t *testing.T) {
	runParity(t, filepath.Join("testdata", "parity", "synthetic"))
}

func TestParityFull(t *testing.T) {
	dir := os.Getenv(parityEnv)
	if dir == "" {
		t.Skipf("%s is not set; the full parity export is local only, so this run covers the synthetic set alone", parityEnv)
	}
	runParity(t, dir)
}
