package guard

import (
	"bytes"
	"encoding/json"
)

// Origins a script can carry in State.ScriptsRun.
const (
	OriginWrittenThisSession = "written_this_session"
	OriginChangedThisSession = "changed_this_session"
	OriginExisting           = "existing"
)

// State is what the guard reads about one proposed command. A bundle chooses
// which of these fields each stage receives, and in what order.
type State struct {
	WorkingDirectory string               `json:"working_directory"`
	Branch           string               `json:"branch"`
	RecentCommands   []string             `json:"recent_commands"`
	ProposedCommand  string               `json:"proposed_command"`
	ScriptsRun       []ScriptEntry        `json:"scripts_run"`
	PackageScripts   []PackageScriptEntry `json:"package_scripts"`
}

// ScriptEntry is one local script a command runs. Content is nil for an
// existing script and for a session script that cannot be read.
type ScriptEntry struct {
	Path    string  `json:"path"`
	Origin  string  `json:"origin"`
	Content *string `json:"content"`
}

// PackageScriptEntry is one npm, pnpm, yarn or bun script a command runs.
// Command is nil when package.json has no such script.
type PackageScriptEntry struct {
	Runner  string              `json:"runner"`
	Script  string              `json:"script"`
	Command *string             `json:"command"`
	Files   []PackageScriptFile `json:"files"`
}

// PackageScriptFile is the start of one local script a package script runs.
// Excerpt is nil when the file cannot be read.
type PackageScriptFile struct {
	Path      string  `json:"path"`
	Excerpt   *string `json:"excerpt"`
	Truncated bool    `json:"truncated"`
}

// normalised returns a copy whose lists are never nil, so they encode as []
// and never as null.
func (s State) normalised() State {
	if s.RecentCommands == nil {
		s.RecentCommands = []string{}
	}
	if s.ScriptsRun == nil {
		s.ScriptsRun = []ScriptEntry{}
	}
	if s.PackageScripts == nil {
		s.PackageScripts = []PackageScriptEntry{}
	} else {
		files := make([]PackageScriptEntry, len(s.PackageScripts))
		copy(files, s.PackageScripts)
		for i := range files {
			if files[i].Files == nil {
				files[i].Files = []PackageScriptFile{}
			}
		}
		s.PackageScripts = files
	}
	return s
}

// The state fields a bundle's stage may list. The names are the keys of the
// JSON a stage receives.
const (
	FieldWorkingDirectory = "working_directory"
	FieldBranch           = "branch"
	FieldRecentCommands   = "recent_commands"
	FieldProposedCommand  = "proposed_command"
	FieldScriptsRun       = "scripts_run"
	FieldPackageScripts   = "package_scripts"
)

var stateFieldNames = []string{
	FieldWorkingDirectory, FieldBranch, FieldRecentCommands,
	FieldProposedCommand, FieldScriptsRun, FieldPackageScripts,
}

func knownField(name string) bool {
	for _, f := range stateFieldNames {
		if f == name {
			return true
		}
	}
	return false
}

// FieldSet is a set of state field names. A nil FieldSet means every field.
type FieldSet map[string]bool

func (f FieldSet) wants(name string) bool { return f == nil || f[name] }

func (s State) fieldValue(name string) any {
	switch name {
	case FieldWorkingDirectory:
		return s.WorkingDirectory
	case FieldBranch:
		return s.Branch
	case FieldRecentCommands:
		return s.RecentCommands
	case FieldProposedCommand:
		return s.ProposedCommand
	case FieldScriptsRun:
		return s.ScriptsRun
	case FieldPackageScripts:
		return s.PackageScripts
	}
	return nil
}

// encode returns the compact JSON object a stage receives: exactly the listed
// fields, in the listed order.
func (s State) encode(fields []string) ([]byte, error) {
	n := s.normalised()
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalNoEscape(f)
		if err != nil {
			return nil, err
		}
		val, err := marshalNoEscape(n.fieldValue(f))
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// indented is encode laid out with a two space indent, the stage 2 user
// message.
func (s State) indented(fields []string) (string, error) {
	compact, err := s.encode(fields)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return "", err
	}
	return out.String(), nil
}

// marshalNoEscape marshals without HTML escaping, so < > & stay literal.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
