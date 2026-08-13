package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// tree writes a set of files, each named by its path relative to a fresh
// temporary root, and returns the root.
func tree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		path := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// relative turns the absolute directories nodeProjectDirs returns back into
// slash-separated paths under root, so the assertions read as the layout.
func relative(t *testing.T, root string, dirs []string) []string {
	t.Helper()
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		rel, err := filepath.Rel(root, d)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	slices.Sort(out)
	return out
}

func TestNodeProjectDirsFindsProjectsBelowTheRepositoryRoot(t *testing.T) {
	// The shape of this repository: no package.json at the root, the frontend
	// one level down. A root-only search finds nothing here, which is the case
	// that sent sessions into `tsc: not found`.
	root := tree(t, "go.mod", "web/package.json", "web/package-lock.json")
	if got, want := relative(t, root, nodeProjectDirs(root)), []string{"web"}; !slices.Equal(got, want) {
		t.Errorf("nodeProjectDirs = %v, want %v", got, want)
	}
}

func TestNodeProjectDirsStopsAtAWorkspaceRoot(t *testing.T) {
	// A workspace root installs its own members. Descending would run a second
	// install inside the tree the first one just populated.
	root := tree(t,
		"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml",
		"packages/api/package.json", "packages/ui/package.json",
	)
	if got, want := relative(t, root, nodeProjectDirs(root)), []string{"."}; !slices.Equal(got, want) {
		t.Errorf("nodeProjectDirs = %v, want %v", got, want)
	}
}

func TestNodeProjectDirsSkipsVendoredAndGeneratedTrees(t *testing.T) {
	// Every one of these carries package.json files belonging to something
	// other than the repository being worked on.
	root := tree(t,
		"web/package.json",
		"web/node_modules/react/package.json",
		"vendor/thing/package.json",
		"third_party/mirror/package.json",
		"testdata/fixture/package.json",
		"dist/package.json",
		".cache/package.json",
	)
	if got, want := relative(t, root, nodeProjectDirs(root)), []string{"web"}; !slices.Equal(got, want) {
		t.Errorf("nodeProjectDirs = %v, want %v", got, want)
	}
}

func TestNodeProjectDirsDoesNotSearchIndefinitely(t *testing.T) {
	root := tree(t, "a/b/c/package.json")
	if got := nodeProjectDirs(root); len(got) != 0 {
		t.Errorf("nodeProjectDirs = %v, want nothing below maxProjectDepth", relative(t, root, got))
	}
}

// The lockfile chooses the manager. Running pnpm against a package-lock.json
// repository writes pnpm-lock.yaml and pnpm-workspace.yaml into the working
// tree, which would then show up in the session's diff.
func TestNodeInstallCommandFollowsTheLockfile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []string
	}{
		{"npm", []string{"package.json", "package-lock.json"}, []string{"npm", "ci"}},
		{"pnpm", []string{"package.json", "pnpm-lock.yaml"}, []string{"pnpm", "install", "--frozen-lockfile"}},
		{"yarn", []string{"package.json", "yarn.lock"}, []string{"yarn", "install"}},
		{"no lockfile", []string{"package.json"}, []string{"npm", "install"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nodeInstallCommand(tree(t, tc.files...))
			if !ok {
				t.Fatal("no install command for a directory holding a package.json")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("nodeInstallCommand = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNodeInstallCommandDeclinesADirectoryWithNoProject(t *testing.T) {
	if got, ok := nodeInstallCommand(tree(t, "main.go")); ok {
		t.Errorf("nodeInstallCommand = %v, want none", got)
	}
}

// A project that already has its dependencies is left alone, so re-preparing
// a workspace costs nothing and a repository that commits node_modules is not
// overwritten.
func TestPendingInstallsSkipsAnAlreadyPopulatedProject(t *testing.T) {
	root := tree(t, "web/package.json", "web/package-lock.json", "web/node_modules/.package-lock.json")
	if got := pendingInstalls(root); len(got) != 0 {
		t.Errorf("pendingInstalls = %v, want nothing", got)
	}
}

func TestPendingInstallsPairsEachProjectWithItsCommand(t *testing.T) {
	root := tree(t, "web/package.json", "web/package-lock.json")
	got := pendingInstalls(root)
	if len(got) != 1 {
		t.Fatalf("pendingInstalls = %v, want one project", got)
	}
	if !slices.Equal(got[0].command, []string{"npm", "ci"}) {
		t.Errorf("command = %v, want npm ci", got[0].command)
	}
	if filepath.Base(got[0].dir) != "web" {
		t.Errorf("dir = %q, want the web project", got[0].dir)
	}
}
