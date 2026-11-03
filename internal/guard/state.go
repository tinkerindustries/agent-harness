package guard

// Origins a script can carry in State.ScriptsRun.
const (
	OriginWrittenThisSession = "written_this_session"
	OriginChangedThisSession = "changed_this_session"
	OriginExisting           = "existing"
)

// State is what the guard reads about one proposed command. The field order is
// the order stage 2 receives them in; stage 1 receives the first five.
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
type PackageScriptFile struct {
	Path      string `json:"path"`
	Excerpt   string `json:"excerpt"`
	Truncated bool   `json:"truncated"`
}

// stage1State is the five fields stage 1 sees. It leaves out package_scripts.
type stage1State struct {
	WorkingDirectory string        `json:"working_directory"`
	Branch           string        `json:"branch"`
	RecentCommands   []string      `json:"recent_commands"`
	ProposedCommand  string        `json:"proposed_command"`
	ScriptsRun       []ScriptEntry `json:"scripts_run"`
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

func (s State) stage1() stage1State {
	n := s.normalised()
	return stage1State{
		WorkingDirectory: n.WorkingDirectory,
		Branch:           n.Branch,
		RecentCommands:   n.RecentCommands,
		ProposedCommand:  n.ProposedCommand,
		ScriptsRun:       n.ScriptsRun,
	}
}

// stage2Text is the user message: the state as JSON indented by 2.
func (s State) stage2Text() (string, error) {
	b, err := marshalIndent(s.normalised(), "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
