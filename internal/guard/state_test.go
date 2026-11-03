package guard

import (
	"context"
	"testing"
	"time"
)

func TestStateEncodeFollowsTheListedFields(t *testing.T) {
	s := State{WorkingDirectory: "/w", Branch: "b", ProposedCommand: "echo <a> & b", PackageScripts: []PackageScriptEntry{{Runner: "npm", Script: "x"}}}
	got, err := s.encode([]string{"proposed_command", "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"proposed_command":"echo <a> & b","branch":"b"}`; string(got) != want {
		t.Fatalf("got %s", got)
	}
	all, err := s.encode(stateFieldNames)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"working_directory":"/w","branch":"b","recent_commands":[],"proposed_command":"echo <a> & b","scripts_run":[],"package_scripts":[{"runner":"npm","script":"x","command":null,"files":[]}]}`; string(all) != want {
		t.Fatalf("got %s", all)
	}
}

func TestStateIndentedIsTwoSpaces(t *testing.T) {
	got, err := State{ProposedCommand: "ls"}.indented([]string{"recent_commands", "proposed_command"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"recent_commands\": [],\n  \"proposed_command\": \"ls\"\n}"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestStateBuilderSkipsUnwantedFields(t *testing.T) {
	dir := t.TempDir()
	fs := &countingFS{}
	b := StateBuilder{FS: fs, Branch: func(context.Context, string) string { t.Error("branch resolved"); return "x" }, Fields: FieldSet{FieldProposedCommand: true}}
	st := b.Build(context.Background(), Inputs{Command: "npm run build", Directory: dir, Commands: []string{"a"}})
	if st.ProposedCommand != "npm run build" || st.Branch != "" || st.RecentCommands != nil || st.ScriptsRun != nil || st.PackageScripts != nil {
		t.Fatalf("state = %+v", st)
	}
	if fs.calls != 0 {
		t.Fatalf("filesystem touched %d times", fs.calls)
	}
	full := StateBuilder{FS: fs, Branch: func(context.Context, string) string { return "x" }}.Build(context.Background(), Inputs{Command: "ls", Directory: dir, Commands: []string{"a"}})
	if full.Branch != "x" || len(full.RecentCommands) != 1 {
		t.Fatalf("full state = %+v", full)
	}
}

type countingFS struct{ calls int }

func (c *countingFS) IsDir(string) bool                     { c.calls++; return false }
func (c *countingFS) IsFile(string) bool                    { c.calls++; return false }
func (c *countingFS) ReadText(string) (string, error)       { c.calls++; return "", errNotUTF8 }
func (c *countingFS) ModTime(string) (t time.Time, ok bool) { c.calls++; return }
