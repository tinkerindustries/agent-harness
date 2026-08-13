package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// newOrigin builds a real repository to clone from, with a commit on main
// and one on a second branch, so the tests below exercise git itself rather
// than a stub.
func newOrigin(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("on main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "first")
	run("checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "FEATURE.md"), []byte("on feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "second")
	run("checkout", "main")
	return dir
}

func TestPrepareClonesDefaultBranchIntoSessionDirectory(t *testing.T) {
	origin := newOrigin(t)
	root := t.TempDir()

	dir, err := Prepare(context.Background(), root, "sess-1", []queue.Repo{{URL: origin}}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := filepath.Join(root, "sess-1"); dir != want {
		t.Fatalf("workspace = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(origin), "README.md")); err != nil {
		t.Fatalf("expected the default branch checked out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(origin), "FEATURE.md")); err == nil {
		t.Fatal("expected main, not feature, with no branch named")
	}
}

func TestPrepareClonesNamedBranch(t *testing.T) {
	origin := newOrigin(t)
	root := t.TempDir()

	dir, err := Prepare(context.Background(), root, "sess-2", []queue.Repo{{URL: origin, Branch: "feature"}}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(origin), "FEATURE.md")); err != nil {
		t.Fatalf("expected the named branch checked out: %v", err)
	}
}

func TestPrepareClonesEveryRepo(t *testing.T) {
	first, second := newOrigin(t), newOrigin(t)
	root := t.TempDir()

	dir, err := Prepare(context.Background(), root, "sess-3", []queue.Repo{{URL: first}, {URL: second}}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, origin := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(dir, filepath.Base(origin), "README.md")); err != nil {
			t.Fatalf("expected %s to be cloned: %v", origin, err)
		}
	}
}

// TestPrepareCreatesScratchDirectory pins that Prepare leaves a scratch/
// directory beside the clones, for files that are not part of the deliverable
// — a screenshot for ReviewScreenshot, a scratch note, a temporary download.
// It lives and dies with the session directory exactly like a clone does.
func TestPrepareCreatesScratchDirectory(t *testing.T) {
	origin := newOrigin(t)
	root := t.TempDir()

	dir, err := Prepare(context.Background(), root, "sess-scratch", []queue.Repo{{URL: origin}}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "scratch")); err != nil {
		t.Fatalf("expected a scratch directory in the session workspace: %v", err)
	} else if !fi.IsDir() {
		t.Fatalf("scratch exists but is not a directory")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(origin), "scratch")); err == nil {
		t.Fatal("scratch must sit beside the clone, not inside it")
	}
}

// A branch that does not exist is the common way a launch gets the repo
// right and the ref wrong, and the run must fail rather than start against
// whatever git left behind.
func TestPrepareFailsOnMissingBranch(t *testing.T) {
	origin := newOrigin(t)
	root := t.TempDir()

	if _, err := Prepare(context.Background(), root, "sess-4", []queue.Repo{{URL: origin, Branch: "nope"}}, nil); err == nil {
		t.Fatal("expected a clone of a branch that does not exist to fail")
	}
}

func TestPrepareRefusesAnExistingSessionDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sess-5"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), root, "sess-5", nil, nil); err == nil {
		t.Fatal("expected an existing session directory to be refused")
	}
}

func TestPrepareRequiresARoot(t *testing.T) {
	if _, err := Prepare(context.Background(), "", "sess-6", nil, nil); err == nil {
		t.Fatal("expected an unconfigured root to be refused")
	}
}

// TestPrepareMaterialisesAttachments pins the scratch/attachments contract:
// the images a request carried land there under their own names, byte for
// byte, before the session starts — so the opening message can name them and
// ReviewScreenshot can read them. An attachment name that is not a plain
// file name is refused rather than written somewhere it could escape.
func TestPrepareMaterialisesAttachments(t *testing.T) {
	root := t.TempDir()

	dir, err := Prepare(context.Background(), root, "sess-att", nil, []Attachment{
		{Name: "mockup.png", MIMEType: "image/png", Data: []byte("the mockup bytes")},
		{Name: "light-theme.webp", MIMEType: "image/webp", Data: []byte("the light theme")},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	for name, want := range map[string]string{
		"mockup.png":       "the mockup bytes",
		"light-theme.webp": "the light theme",
	} {
		data, err := os.ReadFile(filepath.Join(dir, "scratch", "attachments", name))
		if err != nil {
			t.Fatalf("attachment %s was not materialised: %v", name, err)
		}
		if string(data) != want {
			t.Errorf("attachment %s = %q, want %q", name, data, want)
		}
	}

	// A request with no attachments still works, and creates no attachments
	// directory.
	dir2, err := Prepare(context.Background(), root, "sess-noatt", nil, nil)
	if err != nil {
		t.Fatalf("Prepare without attachments: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "scratch", "attachments")); err == nil {
		t.Error("an empty attachment list must not create scratch/attachments")
	}

	// A path-shaped name is refused: an attachment can never escape the
	// attachments directory.
	if _, err := Prepare(context.Background(), root, "sess-badatt", nil, []Attachment{
		{Name: "../escape.png", MIMEType: "image/png", Data: []byte("x")},
	}); err == nil {
		t.Fatal("expected a path-shaped attachment name to be refused")
	}
}
