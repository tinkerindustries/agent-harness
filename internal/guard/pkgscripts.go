package guard

import (
	"encoding/json"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

// FileSystem is the part of the disk package script resolution reads. Paths
// are the strings the resolver builds, so a path may hold a ".." that it has
// not collapsed. A path that is neither a file nor a directory is missing.
type FileSystem interface {
	IsDir(path string) bool
	IsFile(path string) bool
	// ReadText returns a file's text decoded as UTF-8 with CRLF and CR
	// translated to LF, or an error when it cannot be read or is not UTF-8.
	ReadText(path string) (string, error)
}

// OSFileSystem reads the real disk.
type OSFileSystem struct{}

func (OSFileSystem) IsDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func (OSFileSystem) IsFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func (OSFileSystem) ReadText(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errNotUTF8
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

type textError string

func (e textError) Error() string { return string(e) }

const errNotUTF8 = textError("not valid UTF-8")

const (
	maxDepth        = 3
	maxEntries      = 8
	excerptChars    = 1500
	fallbackChars   = 600
	worktreeSegment = "/.claude/worktrees/"
)

var (
	packageRunners   = map[string]bool{"npm": true, "pnpm": true, "yarn": true, "bun": true}
	runCommands      = map[string]bool{"run": true, "run-script": true}
	npmScriptNames   = map[string]bool{"test": true, "start": true, "stop": true, "restart": true}
	directoryOptions = map[string]bool{"-C": true, "--dir": true, "--prefix": true, "--cwd": true}
	valueOptions     = map[string]bool{"--filter": true, "-F": true, "-w": true, "--workspace": true, "--registry": true, "--loglevel": true, "--reporter": true}
	packageBuiltins  = stringSet(
		"add", "audit", "bin", "cache", "ci", "config", "create", "dedupe", "dlx", "doctor", "exec", "fetch", "i",
		"import", "info", "init", "install", "install-test", "link", "list", "ln", "login", "logout", "ls",
		"outdated", "pack", "patch", "prune", "publish", "rebuild", "remove", "rm", "root", "run", "run-script",
		"self-update", "server", "setup", "store", "uninstall", "unlink", "up", "update", "upgrade", "view", "why",
		"x", "workspaces", "whoami", "version", "help", "env", "licenses",
	)
)

func stringSet(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

// ppath is a lexical POSIX path with Python's pathlib normalisation: repeated
// slashes and "." parts collapse, ".." parts stay.
type ppath struct {
	root  string
	parts []string
}

func parsePath(s string) ppath {
	var p ppath
	if strings.HasPrefix(s, "/") {
		p.root = "/"
		if strings.HasPrefix(s, "//") && !strings.HasPrefix(s, "///") {
			p.root = "//"
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part != "" && part != "." {
			p.parts = append(p.parts, part)
		}
	}
	return p
}

func (p ppath) String() string {
	if p.root == "" && len(p.parts) == 0 {
		return "."
	}
	return p.root + strings.Join(p.parts, "/")
}

func (p ppath) isAbs() bool { return p.root != "" }

func (p ppath) parent() ppath {
	if len(p.parts) == 0 {
		return p
	}
	return ppath{root: p.root, parts: p.parts[:len(p.parts)-1]}
}

func (p ppath) join(s string) ppath {
	q := parsePath(s)
	if q.isAbs() {
		return q
	}
	parts := append(append([]string(nil), p.parts...), q.parts...)
	return ppath{root: p.root, parts: parts}
}

// resolved collapses "." and ".." lexically and follows no symlinks.
func (p ppath) resolved() string { return path.Clean(p.String()) }

// packageCall is one package script invocation: runner, script name and the
// directory it was given, nil when none.
type packageCall struct {
	runner string
	script string
	where  *string
}

func (c packageCall) equal(o packageCall) bool {
	if c.runner != o.runner || c.script != o.script || (c.where == nil) != (o.where == nil) {
		return false
	}
	return c.where == nil || *c.where == *o.where
}

// callIn returns the package script the words run.
func callIn(words []string) (packageCall, bool) {
	words = stripPrefix(words)
	if len(words) < 2 || !packageRunners[words[0]] {
		return packageCall{}, false
	}
	runner, rest := words[0], words[1:]
	var directory *string
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		option := rest[0]
		name, value, hasEquals := strings.Cut(option, "=")
		switch {
		case hasEquals && directoryOptions[name]:
			directory = &value
			rest = rest[1:]
		case directoryOptions[option] && len(rest) > 1:
			directory = &rest[1]
			rest = rest[2:]
		case valueOptions[option] && len(rest) > 1:
			rest = rest[2:]
		default:
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return packageCall{}, false
	}
	if runCommands[rest[0]] {
		names := withoutOptions(rest[1:])
		if len(names) == 0 {
			return packageCall{}, false
		}
		return packageCall{runner, names[0], directory}, true
	}
	if runner == "npm" {
		if npmScriptNames[rest[0]] {
			return packageCall{runner, rest[0], directory}, true
		}
		return packageCall{}, false
	}
	if packageBuiltins[rest[0]] || strings.HasPrefix(rest[0], "./") || strings.HasPrefix(rest[0], "/") || strings.HasPrefix(rest[0], "@") {
		return packageCall{}, false
	}
	return packageCall{runner, rest[0], directory}, true
}

// packageScriptCalls returns each package script the command runs, in order
// and without repeats. The directory is the last cd target or -C/--prefix
// value before the call, as written.
func packageScriptCalls(command string) []packageCall {
	var calls []packageCall
	var cwd *string
	for _, segment := range scriptSplit.Split(stripQuoted(stripHeredocs(command)), -1) {
		words := SplitWords(pyStrip(segment))
		if len(words) == 0 {
			continue
		}
		if words[0] == "cd" && len(words) > 1 {
			cwd = &words[1]
			continue
		}
		call, ok := callIn(words)
		if !ok {
			continue
		}
		directory := call.where
		call.where = directory
		if directory == nil {
			call.where = cwd
		} else if cwd != nil && !parsePath(*directory).isAbs() {
			joined := parsePath(*cwd).join(*directory).String()
			call.where = &joined
		}
		seen := false
		for _, c := range calls {
			if c.equal(call) {
				seen = true
				break
			}
		}
		if !seen {
			calls = append(calls, call)
		}
	}
	return calls
}

// packageScripts is the "scripts" object of a package.json. A key whose value
// is not a string, including null, has a nil value.
type packageScripts map[string]*string

// readPackageScripts parses package.json text. Text that is not a JSON object
// has no scripts.
func readPackageScripts(text string) packageScripts {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(replaceNonFinite(text)), &top) != nil {
		return packageScripts{}
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(top["scripts"], &raw) != nil {
		return packageScripts{}
	}
	scripts := make(packageScripts, len(raw))
	for name, value := range raw {
		var line *string
		if json.Unmarshal(value, &line) != nil {
			line = nil
		}
		scripts[name] = line
	}
	return scripts
}

// replaceNonFinite swaps the bare literals NaN, Infinity and -Infinity, which
// Python's json module accepts and encoding/json rejects, for 0.
func replaceNonFinite(text string) string {
	if !strings.Contains(text, "NaN") && !strings.Contains(text, "Infinity") {
		return text
	}
	var out strings.Builder
	inString := false
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case inString:
			out.WriteByte(c)
			i++
			if c == '\\' && i < len(text) {
				out.WriteByte(text[i])
				i++
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
			out.WriteByte(c)
			i++
		case strings.HasPrefix(text[i:], "NaN"):
			out.WriteByte('0')
			i += len("NaN")
		case strings.HasPrefix(text[i:], "-Infinity"):
			out.WriteByte('0')
			i += len("-Infinity")
		case strings.HasPrefix(text[i:], "Infinity"):
			out.WriteByte('0')
			i += len("Infinity")
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// locateDirectory finds the directory a call runs in, falling back to the
// repository when its worktree is gone.
func locateDirectory(fsys FileSystem, workingDirectory string, where *string, repositories []string) (ppath, bool) {
	base := parsePath(workingDirectory)
	dir := base
	if where != nil {
		if given := parsePath(*where); given.isAbs() {
			dir = given
		} else {
			dir = base.join(*where)
		}
	}
	if fsys.IsDir(dir.String()) {
		return dir, true
	}
	text := dir.String()
	if root, rest, found := strings.Cut(text, worktreeSegment); found {
		remainder := ""
		if _, after, ok := strings.Cut(rest, "/"); ok {
			remainder = after
		}
		candidate := parsePath(root).join(remainder)
		if fsys.IsDir(candidate.String()) {
			return candidate, true
		}
	}
	for _, repository := range repositories {
		if strings.HasPrefix(text, repository) && fsys.IsDir(parsePath(repository).String()) {
			return parsePath(repository), true
		}
	}
	return ppath{}, false
}

// findPackageJSON looks for package.json in the directory and then each parent.
func findPackageJSON(fsys FileSystem, directory ppath) (ppath, packageScripts, bool) {
	for folder := directory; ; folder = folder.parent() {
		candidate := folder.join("package.json").String()
		if fsys.IsFile(candidate) {
			text, err := fsys.ReadText(candidate)
			if err != nil {
				return folder, packageScripts{}, true
			}
			return folder, readPackageScripts(text), true
		}
		if len(folder.parent().parts) == len(folder.parts) {
			break
		}
	}
	return ppath{}, packageScripts{}, false
}

// leadingComment is the comment block at the top of a script, after any
// shebang line.
func leadingComment(text string) string {
	lines := pySplitLines(text)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
		lines = lines[1:]
	}
	var head []string
	inBlock := false
	for _, line := range lines {
		stripped := pyStrip(line)
		if inBlock {
			head = append(head, line)
			if strings.Contains(stripped, "*/") || strings.HasSuffix(stripped, `"""`) || strings.HasSuffix(stripped, "'''") {
				inBlock = false
			}
			continue
		}
		if len(head) == 0 && stripped == "" {
			continue
		}
		switch {
		case strings.HasPrefix(stripped, "/*") || strings.HasPrefix(stripped, `"""`) || strings.HasPrefix(stripped, "'''"):
			head = append(head, line)
			closed := strings.Count(stripped, "*/") > 0 || strings.Count(stripped, `"""`) >= 2 || strings.Count(stripped, "'''") >= 2
			inBlock = !closed
		case strings.HasPrefix(stripped, "//") || strings.HasPrefix(stripped, "#"):
			head = append(head, line)
		default:
			return strings.Join(head, "\n")
		}
	}
	return strings.Join(head, "\n")
}

// excerpt is the head of a script file: its leading comment block, else its
// first lines, capped. The text is nil when the file cannot be read.
func excerpt(fsys FileSystem, p string) (*string, bool) {
	if !fsys.IsFile(p) {
		return nil, false
	}
	text, err := fsys.ReadText(p)
	if err != nil {
		return nil, false
	}
	head := leadingComment(text)
	limit := excerptChars
	if head == "" {
		limit = fallbackChars
		head = text
	}
	shown := []rune(head)
	if len(shown) > limit {
		shown = shown[:limit]
		if cut := lastRune(shown, '\n'); cut >= 0 {
			shown = shown[:cut]
		}
	}
	shownText := pyRstrip(string(shown))
	truncated := utf8.RuneCountInString(pyRstrip(text)) > utf8.RuneCountInString(shownText)
	return &shownText, truncated
}

func lastRune(r []rune, c rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == c {
			return i
		}
	}
	return -1
}

// scriptFiles reads the head of each local script a package script line runs.
func scriptFiles(fsys FileSystem, line string, folder ppath, shown map[string]bool) []PackageScriptFile {
	found := []PackageScriptFile{}
	for _, name := range ExecutedScripts(line) {
		var p string
		if given := parsePath(name); given.isAbs() {
			p = given.String()
		} else {
			p = folder.join(name).resolved()
		}
		if shown[p] {
			continue
		}
		shown[p] = true
		text, truncated := excerpt(fsys, p)
		found = append(found, PackageScriptFile{Path: name, Excerpt: text, Truncated: truncated})
	}
	return found
}

// ResolvePackageScripts finds the npm, pnpm, yarn and bun scripts the command
// runs, reads package.json for each, follows scripts that call other scripts
// and attaches the head of each local script file they run. It only reads.
// working directory is the effective directory of the call, and repositories
// are the roots to fall back to when a worktree is gone.
func ResolvePackageScripts(fsys FileSystem, command, workingDirectory string, repositories []string) []PackageScriptEntry {
	var entries []*PackageScriptEntry
	seen := map[string]bool{}
	shown := map[string]bool{}

	var visit func(runner, script string, where *string, depth int)
	visit = func(runner, script string, where *string, depth int) {
		if len(entries) >= maxEntries {
			return
		}
		var folder ppath
		var scripts packageScripts
		found := false
		if directory, ok := locateDirectory(fsys, workingDirectory, where, repositories); ok {
			folder, scripts, found = findPackageJSON(fsys, directory)
		}
		folderKey := "None"
		if found {
			folderKey = folder.String()
		}
		key := folderKey + "\x00" + script
		if seen[key] {
			return
		}
		seen[key] = true
		line := scripts[script]
		entry := &PackageScriptEntry{Runner: runner, Script: script, Command: line, Files: []PackageScriptFile{}}
		entries = append(entries, entry)
		if runner == "npm" || runner == "yarn" {
			for _, hook := range []string{"pre" + script, "post" + script} {
				if _, ok := scripts[hook]; ok {
					folderText := folder.String()
					visit(runner, hook, &folderText, depth)
				}
			}
		}
		if line == nil || !found {
			return
		}
		entry.Files = scriptFiles(fsys, *line, folder, shown)
		if depth < maxDepth {
			for _, nested := range packageScriptCalls(*line) {
				nestedWhere := nested.where
				if nestedWhere == nil || *nestedWhere == "" {
					folderText := folder.String()
					nestedWhere = &folderText
				}
				visit(nested.runner, nested.script, nestedWhere, depth+1)
			}
		}
	}

	for _, call := range packageScriptCalls(command) {
		visit(call.runner, call.script, call.where, 1)
	}
	result := make([]PackageScriptEntry, len(entries))
	for i, entry := range entries {
		result[i] = *entry
	}
	return result
}
