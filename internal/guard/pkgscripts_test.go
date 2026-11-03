package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPythonPathNormalisation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "."},
		{"a//b/./c/", "a/b/c"},
		{"/a/b/../c", "/a/b/../c"},
		{"//a", "//a"},
		{"///a", "/a"},
	}
	for _, c := range cases {
		if got := parsePath(c.in).String(); got != c.want {
			t.Errorf("parsePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := parsePath("/work/app").join("../x").String(); got != "/work/app/../x" {
		t.Errorf("join kept = %q", got)
	}
	if got := parsePath("/work/app").join("/abs").String(); got != "/abs" {
		t.Errorf("join absolute = %q", got)
	}
	if got := parsePath("/work/app/..").parent().String(); got != "/work/app" {
		t.Errorf("parent of a .. part = %q", got)
	}
}

func TestOSFileSystem(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var fsys FileSystem = OSFileSystem{}
	text, err := fsys.ReadText(write("crlf", []byte("a\r\nb\rc\n")))
	if err != nil || text != "a\nb\nc\n" {
		t.Errorf("ReadText = %q, %v", text, err)
	}
	bad := write("bad", []byte{0xff, 0xfe})
	if !fsys.IsFile(bad) {
		t.Error("an unreadable file is still a file")
	}
	if _, err := fsys.ReadText(bad); err == nil {
		t.Error("invalid UTF-8 read without error")
	}
	if !fsys.IsDir(dir) || fsys.IsFile(dir) || fsys.IsDir(bad) || fsys.IsFile(filepath.Join(dir, "none")) {
		t.Error("IsDir and IsFile disagree with the disk")
	}
}

func TestResolvePackageScriptsOnDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `{"scripts": {"release": "node scripts/release.mjs"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "release.mjs"), []byte("// Uploads a build.\nconsole.log(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ResolvePackageScripts(OSFileSystem{}, "pnpm release", dir, []string{dir})
	if len(got) != 1 || got[0].Command == nil || *got[0].Command != "node scripts/release.mjs" {
		t.Fatalf("entries = %+v", got)
	}
	files := got[0].Files
	if len(files) != 1 || files[0].Excerpt == nil || *files[0].Excerpt != "// Uploads a build." || !files[0].Truncated {
		t.Errorf("files = %+v", files)
	}
}

func TestRulesIdentityIsStable(t *testing.T) {
	a, b := RulesIdentity(), RulesIdentity()
	if a == "" || a != b {
		t.Errorf("identity %q then %q", a, b)
	}
	if guardRulesSource == "" || scriptDetectionSource == "" || packageScriptsSource == "" {
		t.Error("a ported source is not embedded")
	}
}

var (
	_ RuleChecker = Rules{}
	_ Escalator   = ScriptDetector{}
)
