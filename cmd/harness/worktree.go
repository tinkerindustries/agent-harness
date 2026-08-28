package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/worktree"
)

const worktreeUsage = `usage: harness worktree <command> [flags]

commands:
  init [-slug NAME] [-dry-run] [-force] [-standalone]
                                           allocate this worktree a slot, write
                                           .worktree-env.xml and update .env
  show [path]                             print a worktree's descriptor
  list                                    every registered worktree
  rm <slug> [-dry-run]                    tear down a worktree's containers,
                                           volumes and network, and free its slot
  seed [-from CONTAINER] [-dry-run]       copy the credential settings from the
                                           main checkout's running stack into
                                           this worktree's, after compose is up
  doctor                                  report registry/filesystem drift

Run "init" from inside the worktree, after it exists. Run "rm" before
"git worktree remove" — see docs/WORKTREES.md and
.claude/skills/worktree-create, .claude/skills/worktree-remove.

-standalone treats a plain "git clone" (no sibling main checkout) as
allocatable too, for a disposable workspace with no linked-worktree
relationship to anything else — a deepseek-flash-task agent's own clone, not
your primary checkout of this repo. Git cannot tell the two apart on its own;
this flag is what tells "init" which one you mean.`

func runWorktree(ctx context.Context, args []string) error {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, worktreeUsage)
		return errors.New("worktree: missing subcommand")
	}
	switch args[0] {
	case "init":
		return runWorktreeInit(args[1:])
	case "show":
		return runWorktreeShow(args[1:])
	case "list":
		return runWorktreeList(args[1:])
	case "rm":
		return runWorktreeRm(args[1:])
	case "seed":
		return runWorktreeSeed(ctx, args[1:])
	case "doctor":
		return runWorktreeDoctor(args[1:])
	case "-h", "-help", "--help", "help":
		fmt.Println(worktreeUsage)
		return nil
	default:
		fmt.Fprintln(os.Stderr, worktreeUsage)
		return fmt.Errorf("worktree: unknown subcommand %q", args[0])
	}
}

func runWorktreeInit(args []string) error {
	fs := flag.NewFlagSet("worktree init", flag.ContinueOnError)
	slugFlag := fs.String("slug", "", "worktree slug (default: the worktree directory's name)")
	dryRun := fs.Bool("dry-run", false, "compute and print the allocation without writing anything")
	force := fs.Bool("force", false, "reassign the slug even if it is registered to a different path")
	standalone := fs.Bool("standalone", false, "allocate a plain git clone with no sibling main checkout — for a disposable agent workspace, never your primary checkout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := worktree.Root()
	if err != nil {
		return fmt.Errorf("worktree init: %w (run this from inside a git worktree)", err)
	}
	if !*standalone {
		isMain, err := worktree.IsMainWorktree(root)
		if err != nil {
			return err
		}
		if isMain {
			return errors.New("worktree init: this is the main checkout (slot 0) — it already uses docker-compose.yml's default ports and needs no manifest; run this from a linked worktree instead, or pass -standalone if this is actually a disposable clone with no sibling main checkout")
		}
	}

	slug := *slugFlag
	if slug == "" {
		slug = filepath.Base(root)
	}
	if err := worktree.ValidateSlug(slug); err != nil {
		return fmt.Errorf("worktree init: %w", err)
	}
	branch, err := worktree.CurrentBranch(root)
	if err != nil {
		return err
	}
	mainRoot, err := worktree.MainRoot(root)
	if err != nil {
		return err
	}

	var entry worktree.Entry
	var reused bool

	lockErr := worktree.WithLock(func() error {
		reg, err := worktree.LoadRegistry()
		if err != nil {
			return err
		}

		if existing, ok := reg.Worktrees[slug]; ok {
			if existing.Path != root && !*force {
				return fmt.Errorf("slug %q is already registered to %s (pass -force to reassign it, or pick a different -slug)", slug, existing.Path)
			}
			if existing.Path == root {
				entry = existing
				reused = true
				return nil
			}
		}
		for s, e := range reg.Worktrees {
			if e.Path == root && s != slug {
				return fmt.Errorf("this worktree is already registered as slug %q (slot %d) — delete that entry first, or re-run init with -slug %s", s, e.Slot, s)
			}
		}

		slot, ports, err := worktree.AllocateSlot(reg)
		if err != nil {
			return err
		}
		entry = worktree.Entry{
			Slug:              slug,
			Path:              root,
			Slot:              slot,
			Branch:            branch,
			ComposeProjectDev: fmt.Sprintf("deepseek-harness-%s", slug),
			// Matches scripts/test.sh's own derivation: ${COMPOSE_PROJECT_NAME}-test.
			ComposeProjectTst: fmt.Sprintf("deepseek-harness-%s-test", slug),
			Ports:             ports,
			CreatedAt:         time.Now().UTC().Format(time.RFC3339),
		}
		if *dryRun {
			return nil
		}
		reg.Worktrees[slug] = entry
		return reg.Save()
	})
	if lockErr != nil {
		return fmt.Errorf("worktree init: %w", lockErr)
	}

	d := worktree.NewDescriptor(entry.Slug, entry.Path, entry.Branch, entry.CreatedAt, entry.Slot, entry.Ports)

	if *dryRun {
		fmt.Println("dry run — nothing written:")
		printWorktreeSummary(d, false)
		return nil
	}

	if err := worktree.WriteDescriptor(root, d); err != nil {
		return fmt.Errorf("worktree init: %w", err)
	}
	if err := worktree.UpsertEnv(root, d, filepath.Join(mainRoot, ".env")); err != nil {
		return fmt.Errorf("worktree init: %w", err)
	}

	if reused {
		fmt.Println("already initialised — reconciled descriptor and .env against the existing slot:")
	} else {
		fmt.Println("allocated:")
	}
	printWorktreeSummary(d, true)
	return nil
}

func printWorktreeSummary(d worktree.Descriptor, withNextSteps bool) {
	fmt.Printf("  slug             %s\n", d.Identity.Slug)
	fmt.Printf("  slot             %d\n", d.Identity.Slot)
	fmt.Printf("  path             %s\n", d.Identity.Path)
	if d.Identity.Branch != "" {
		fmt.Printf("  branch           %s\n", d.Identity.Branch)
	}
	fmt.Printf("  dev compose      %s\n", d.Compose.DevProjectName)
	fmt.Printf("  test compose     %s\n", d.Compose.TestProjectName)
	fmt.Println("  ports:")
	fmt.Printf("    harness http   %d   (http://127.0.0.1:%d, /mcp included)\n", d.Ports.HarnessHTTP, d.Ports.HarnessHTTP)
	fmt.Printf("    vite dev       %d\n", d.Ports.Vite)
	if len(d.Shared) > 0 {
		fmt.Println("  shared (not isolated by this tool):")
		for _, s := range d.Shared {
			fmt.Printf("    - %s: %s\n", s.Name, s.Impact)
		}
	}
	if withNextSteps {
		fmt.Println()
		fmt.Printf("  next: docker compose up -d --build   (reads the .env this just wrote)\n")
		fmt.Printf("        harness worktree seed          (copy the API keys and GitHub credential\n")
		fmt.Printf("                                        from the main checkout's stack, once it is up)\n")
	}
}

func runWorktreeShow(args []string) error {
	fs := flag.NewFlagSet("worktree show", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := fs.Arg(0)
	if root == "" {
		var err error
		root, err = worktree.Root()
		if err != nil {
			return fmt.Errorf("worktree show: %w", err)
		}
	}
	d, err := worktree.ReadDescriptor(root)
	if err != nil {
		return fmt.Errorf("worktree show: %w (has `harness worktree init` run here?)", err)
	}
	printWorktreeSummary(d, false)
	return nil
}

func runWorktreeList(args []string) error {
	fs := flag.NewFlagSet("worktree list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	reg, err := worktree.LoadRegistry()
	if err != nil {
		return fmt.Errorf("worktree list: %w", err)
	}
	if len(reg.Worktrees) == 0 {
		fmt.Println("no worktrees registered (run `harness worktree init` from inside one)")
		return nil
	}
	slugs := make([]string, 0, len(reg.Worktrees))
	for s := range reg.Worktrees {
		slugs = append(slugs, s)
	}
	sort.Slice(slugs, func(i, j int) bool { return reg.Worktrees[slugs[i]].Slot < reg.Worktrees[slugs[j]].Slot })

	fmt.Printf("%-4s %-24s %-8s %-6s %s\n", "slot", "slug", "http", "gone?", "path")
	for _, s := range slugs {
		e := reg.Worktrees[s]
		gone := ""
		if _, err := os.Stat(e.Path); os.IsNotExist(err) {
			gone = "!"
		}
		fmt.Printf("%-4d %-24s %-8d %-6s %s\n", e.Slot, e.Slug, e.Ports.HarnessHTTP, gone, e.Path)
	}
	return nil
}

func runWorktreeRm(args []string) error {
	fs := flag.NewFlagSet("worktree rm", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print what would be torn down without touching anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: harness worktree rm <slug> [-dry-run]")
	}
	slug := fs.Arg(0)

	var (
		entry  worktree.Entry
		notes  []string
		failed bool
	)

	err := worktree.WithLock(func() error {
		reg, err := worktree.LoadRegistry()
		if err != nil {
			return err
		}
		e, ok := reg.Worktrees[slug]
		if !ok {
			return fmt.Errorf("no worktree registered as slug %q (see `harness worktree list`)", slug)
		}
		entry = e

		if *dryRun {
			return nil
		}

		for _, project := range []string{entry.ComposeProjectDev, entry.ComposeProjectTst} {
			projNotes, err := dockerTeardownProject(project)
			notes = append(notes, projNotes...)
			if err != nil {
				failed = true
				notes = append(notes, fmt.Sprintf("%s: %v", project, err))
			}
		}
		if failed {
			// Leave the entry so a re-run can finish the job — see
			// allocation-design.md's teardown contract.
			return fmt.Errorf("teardown left resources behind (see above); the registry entry for %q was NOT removed — fix docker and re-run `harness worktree rm %s`", slug, slug)
		}
		delete(reg.Worktrees, slug)
		return reg.Save()
	})

	for _, n := range notes {
		fmt.Println("  " + n)
	}
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}

	if *dryRun {
		fmt.Printf("dry run — would tear down %s and %s (slot %d), then free the slug %q\n",
			entry.ComposeProjectDev, entry.ComposeProjectTst, entry.Slot, slug)
		return nil
	}

	fmt.Printf("freed slot %d (%s)\n", entry.Slot, slug)
	if _, statErr := os.Stat(entry.Path); statErr == nil {
		fmt.Printf("the worktree directory still exists — finish with:\n  git worktree remove %s\n", entry.Path)
	}
	return nil
}

// dockerTeardownProject removes every container, network, and volume
// docker compose labelled with the given project name. It works from the
// project name alone — no compose file, no worktree directory — which is
// what lets `rm` run after `git worktree remove` already deleted the tree
// (allocation-design.md: "works from the registry alone").
func dockerTeardownProject(project string) ([]string, error) {
	var notes []string
	filter := "label=com.docker.compose.project=" + project

	steps := []struct {
		kind string
		list []string
		rm   func(id string) []string
	}{
		{"container", []string{"ps", "-a", "-q", "--filter", filter}, func(id string) []string { return []string{"rm", "-f", id} }},
		{"network", []string{"network", "ls", "-q", "--filter", filter}, func(id string) []string { return []string{"network", "rm", id} }},
		{"volume", []string{"volume", "ls", "-q", "--filter", filter}, func(id string) []string { return []string{"volume", "rm", id} }},
	}

	var firstErr error
	for _, step := range steps {
		out, err := exec.Command("docker", step.list...).Output()
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: could not list %ss: %v", project, step.kind, err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			if _, err := exec.Command("docker", step.rm(id)...).CombinedOutput(); err != nil {
				notes = append(notes, fmt.Sprintf("%s: could not remove %s %s: %v", project, step.kind, id, err))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			notes = append(notes, fmt.Sprintf("%s: removed %s %s", project, step.kind, id))
		}
	}
	return notes, firstErr
}

func runWorktreeDoctor(args []string) error {
	fs := flag.NewFlagSet("worktree doctor", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	reg, err := worktree.LoadRegistry()
	if err != nil {
		return fmt.Errorf("worktree doctor: %w", err)
	}

	clean := true
	report := func(format string, args ...any) {
		clean = false
		fmt.Printf(format+"\n", args...)
	}

	knownPaths := map[string]bool{}
	for slug, e := range reg.Worktrees {
		knownPaths[filepath.Clean(e.Path)] = true
		if _, err := os.Stat(e.Path); os.IsNotExist(err) {
			report("stale entry: %q (slot %d) points at %s, which no longer exists — run `harness worktree rm %s`", slug, e.Slot, e.Path, slug)
			continue
		}
		if _, err := worktree.ReadDescriptor(e.Path); err != nil {
			report("drift: %q (slot %d) has no readable %s at %s — re-run `harness worktree init` there", slug, e.Slot, worktree.DescriptorFilename, e.Path)
		}
	}

	// Orphan worktrees: directories under .claude/worktrees/ (from whichever
	// checkout we can resolve) with no registry entry at all.
	if root, err := worktree.Root(); err == nil {
		repoRoot := root
		if isMain, err := worktree.IsMainWorktree(root); err == nil && !isMain {
			if mr, err := worktree.MainRoot(root); err == nil {
				repoRoot = mr
			}
		}
		wtDir := filepath.Join(repoRoot, ".claude", "worktrees")
		if entries, err := os.ReadDir(wtDir); err == nil {
			for _, de := range entries {
				if !de.IsDir() {
					continue
				}
				p := filepath.Clean(filepath.Join(wtDir, de.Name()))
				if !knownPaths[p] {
					report("orphan directory: %s has no registry entry — either `harness worktree init` there, or remove it with `git worktree remove`", p)
				}
			}
		}
	}

	if clean {
		fmt.Println("clean: every registry entry has a live directory and descriptor; no orphan worktree directories found")
	}
	return nil
}
