package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/worktree"
)

// `harness worktree seed`: copy an operator's credentials from an existing
// stack into this worktree's, so a new worktree does not start by asking for
// the same API keys and the same GitHub App private key that every other one
// already has (docs/WORKTREES.md, "Seeding a worktree's credentials").
//
// Only the credentials travel. The rest of the store — the work queue,
// sessions, events, workspace leases, the MCP server registry — stays where
// it is, and deliberately: a worktree that inherited work_queue rows would
// claim and run work that was queued for another stack, against a workspace
// root it does not own.
//
// Why it is its own command rather than part of `init`. A stack's store
// lives in that compose project's own docker volume, which does not exist
// until compose has run once, and `init` runs before that — it is what
// writes the .env compose reads. So seeding happens after the stack is up,
// which is also when the target harness is running to accept the writes.
//
// Why it writes through the HTTP API rather than into the target's SQLite
// file. The running harness validates every write through the settings
// registry, and re-syncs the GitHub credential on the ones that need it
// (internal/githubauth via httpapi.Server.OnSettingChanged), so a seeded App
// key is live immediately. A write into the file behind the process's back
// would skip both.

const worktreeSeedUsage = `usage: harness worktree seed [-from CONTAINER] [-dry-run] [-overwrite]

Copy the credential settings (the API keys and the GitHub credential) from a
running harness into this worktree's, so a new worktree does not need them
typed in again. Run it from inside the worktree, after "docker compose up".

  -from CONTAINER   read from this container instead of the main checkout's
                    stack (default: <main checkout dir>-harness-1)
  -dry-run          print what would be seeded, and change nothing
  -overwrite        replace values already set here (default: leave them)

Values are never printed. http.control_token is never copied: it is generated
per installation and guards that installation's own run-control endpoints.`

// seedExportUsage documents the mode that runs inside the source container.
// It is a deliberate credential-reading primitive and says so; it needs the
// data directory to read, which is already the whole secret.
const seedExportUsage = `usage: harness worktree seed -export

Print this data directory's credential settings as JSON, for "worktree seed"
running on the host to read over docker exec. Not a command to run by hand.`

// runWorktreeSeed dispatches the two halves: -export runs inside the source
// container and reads, everything else runs on the host and writes.
func runWorktreeSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worktree seed", flag.ContinueOnError)
	from := fs.String("from", "", "container to read the credentials from")
	dryRun := fs.Bool("dry-run", false, "print what would be seeded without writing")
	overwrite := fs.Bool("overwrite", false, "replace credentials already set in this worktree")
	export := fs.Bool("export", false, "print this data directory's credentials as JSON (runs inside the source container)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, worktreeSeedUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *export {
		return exportCredentials(ctx)
	}
	return seedFromContainer(ctx, *from, *dryRun, *overwrite)
}

// exportCredentials prints the stored credential settings as a JSON object,
// omitting any that are unset. It runs inside the source container, where
// the data directory is the volume the source stack keeps its store in.
func exportCredentials(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	res := settings.NewResolver(st)

	out := map[string]string{}
	for _, key := range settings.SeedableCredentialKeys() {
		value, ok, err := res.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("worktree seed -export: read %s: %w", key, err)
		}
		if ok && value != "" {
			out[key] = value
		}
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

// seedFromContainer reads the source stack's credentials and writes the ones
// this worktree is missing.
func seedFromContainer(ctx context.Context, from string, dryRun, overwrite bool) error {
	root, err := worktree.Root()
	if err != nil {
		return fmt.Errorf("worktree seed: %w (run this from inside a git worktree)", err)
	}
	d, err := worktree.ReadDescriptor(root)
	if err != nil {
		return fmt.Errorf("worktree seed: %w (run `harness worktree init` first)", err)
	}
	target := fmt.Sprintf("http://127.0.0.1:%d", d.Ports.HarnessHTTP)

	if from == "" {
		mainRoot, err := worktree.MainRoot(root)
		if err != nil {
			return fmt.Errorf("worktree seed: %w (pass -from to name the source container)", err)
		}
		from = defaultSourceContainer(mainRoot)
	}

	source, err := readSourceCredentials(ctx, from)
	if err != nil {
		return err
	}
	alreadySet, err := credentialsSetAt(ctx, target)
	if err != nil {
		return err
	}

	plans := planSeed(settings.SeedableCredentialKeys(), source, alreadySet, overwrite)
	seeded := 0
	for _, p := range plans {
		if p.Action != seedActionWrite {
			continue
		}
		if !dryRun {
			if err := putSetting(ctx, target, p.Key, source[p.Key]); err != nil {
				return err
			}
		}
		seeded++
	}

	if dryRun {
		fmt.Printf("dry run — nothing written. from %s into %s:\n", from, target)
	} else {
		fmt.Printf("seeded %d of %d credentials from %s into %s:\n", seeded, len(plans), from, target)
	}
	for _, p := range plans {
		fmt.Printf("  %-28s %s\n", p.Key, p.Action)
	}
	if seeded == 0 && !dryRun {
		fmt.Println("\nnothing to do — this worktree already has every credential the source has")
	}
	return nil
}

// The three outcomes for one key. Kept as constants because they are printed
// and asserted on.
const (
	seedActionWrite     = "seeded"
	seedActionHave      = "left alone (already set here)"
	seedActionNotSource = "skipped (not set at the source)"
)

// seedPlan is one key's outcome.
type seedPlan struct {
	Key    string
	Action string
}

// planSeed decides what happens to each key. A key already set here is left
// alone unless overwrite is given: the same contract githubauth.SeedFromEnv
// carries, and for the same reason — an operator who deliberately set a
// different value in this worktree should not have it replaced by a copy of
// the main stack's.
func planSeed(keys []string, source map[string]string, alreadySet map[string]bool, overwrite bool) []seedPlan {
	plans := make([]seedPlan, 0, len(keys))
	for _, key := range keys {
		switch {
		case source[key] == "":
			plans = append(plans, seedPlan{key, seedActionNotSource})
		case alreadySet[key] && !overwrite:
			plans = append(plans, seedPlan{key, seedActionHave})
		default:
			plans = append(plans, seedPlan{key, seedActionWrite})
		}
	}
	return plans
}

// defaultSourceContainer names the main checkout's harness container, which
// is where an operator's credentials already are. Compose derives a project
// name from the directory it runs in when nothing overrides it, and names a
// service's first container <project>-<service>-1; the main checkout runs no
// `worktree init`, so nothing overrides it there. A stack that does not
// follow that (a renamed project, or a source that is not the main checkout)
// is what -from is for.
func defaultSourceContainer(mainRoot string) string {
	project := strings.ToLower(filepath.Base(mainRoot))
	project = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return -1
	}, project)
	return project + "-harness-1"
}

// readSourceCredentials runs this same binary inside the source container,
// where the source stack's data volume is mounted, and decodes what it
// prints. docker exec rather than a container of our own with the volume
// mounted: the source stack is running (that is where the credentials are
// being kept), its binary is the one that understands its own store, and
// this needs no image name, no volume name, and no second copy of anything.
func readSourceCredentials(ctx context.Context, container string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "harness", "worktree", "seed", "-export")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		// A stack built before `seed` existed answers by printing the
		// worktree usage, because its binary does not know the subcommand.
		// That is a whole screen of text that says nothing about the actual
		// problem, and it is the expected state of every stack that has not
		// been rebuilt since — worth naming rather than passing through.
		if strings.Contains(message, "usage: harness worktree") || strings.Contains(message, "unknown command") {
			return nil, fmt.Errorf("worktree seed: %s is running a harness with no `worktree seed` in it, so there is nothing to read the credentials with.\n"+
				"Rebuild that stack (`docker compose up -d --build` in its checkout) and run this again.", container)
		}
		return nil, fmt.Errorf("worktree seed: reading credentials from %s failed: %s\n"+
			"Is that stack up? `docker ps` should list it, and -from names a different container.", container, message)
	}
	var out map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("worktree seed: %s returned something that is not the expected JSON: %w", container, err)
	}
	return out, nil
}

// credentialsSetAt asks the target harness which credentials it already
// holds. The settings endpoint masks secret values, which is all this needs
// — whether a key is set, never what it is.
func credentialsSetAt(ctx context.Context, base string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/settings", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("worktree seed: cannot reach this worktree's harness at %s: %w\n"+
			"Start it with `docker compose up -d --build` and run this again.", base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worktree seed: %s/api/settings answered %d", base, resp.StatusCode)
	}
	var entries []struct {
		Key string `json:"key"`
		Set bool   `json:"set"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("worktree seed: unparseable settings from %s: %w", base, err)
	}
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		set[e.Key] = e.Set
	}
	return set, nil
}

// putSetting writes one credential into the target harness through the same
// endpoint the settings screen writes through, so the value is validated and
// anything that has to react to it does.
func putSetting(ctx context.Context, base, key, value string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/api/settings/"+key, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("worktree seed: writing %s: %w", key, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The body may name why the registry refused it; it never contains
		// the value, which the endpoint does not echo.
		return fmt.Errorf("worktree seed: writing %s answered %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
