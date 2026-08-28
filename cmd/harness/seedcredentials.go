package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// `harness seed-credentials`: the two halves of copying an operator's
// credentials from one stack's store into another's, each half running inside
// the stack it touches.
//
// It exists as a top-level command rather than under `worktree` because it is
// not about worktrees. It is about two harness installations, one of which
// already has the API keys and the GitHub App private key that the other
// needs; the worktree case is only the common reason for there to be two.
// scripts/wt-seed.sh is what pairs the halves, and wt.yaml's seed hook is
// what runs it.
//
// Only the credentials travel — settings.SeedableCredentialKeys, which the
// registry derives, so a credential added later is carried without anyone
// remembering this file. http.control_token is excluded there: it is
// generated per installation and guards that installation's own run-control
// endpoints, so a copy would make one stack's bearer token work on another.
//
// The rest of the store never moves, deliberately. A stack that inherited
// work_queue rows would claim and run work queued for another stack, against
// a workspace root it does not own, and a copied lease would point at a
// directory that is not its own.

const seedCredentialsUsage = `usage: harness seed-credentials -export
       harness seed-credentials -import [-dry-run] [-overwrite]

Copy the credential settings (the API keys and the GitHub credential) between
two harness installations. Each half runs inside the stack it touches, so
neither needs the other's data directory mounted.

  -export      print this data directory's credentials as JSON, on stdout
  -import      read that JSON on stdin and write it into this installation
               through its own HTTP API
  -dry-run     with -import: print what would be written, and change nothing
  -overwrite   with -import: replace values already set here (default: leave
               them, so a deliberately different key here is not clobbered)

Values are never printed by either half. Pair them with a pipe — see
scripts/wt-seed.sh, which wt.yaml's seed hook runs for a new worktree.`

// runSeedCredentials dispatches the two halves.
func runSeedCredentials(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed-credentials", flag.ContinueOnError)
	export := fs.Bool("export", false, "print this data directory's credentials as JSON")
	imprt := fs.Bool("import", false, "read credentials as JSON on stdin and write them here")
	dryRun := fs.Bool("dry-run", false, "with -import: print what would be written without writing")
	overwrite := fs.Bool("overwrite", false, "with -import: replace credentials already set here")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, seedCredentialsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *export && *imprt:
		fmt.Fprintln(os.Stderr, seedCredentialsUsage)
		return errors.New("seed-credentials: -export and -import are the two halves and cannot both run here")
	case *export:
		return exportCredentials(ctx)
	case *imprt:
		return importCredentials(ctx, *dryRun, *overwrite)
	default:
		fmt.Fprintln(os.Stderr, seedCredentialsUsage)
		return errors.New("seed-credentials: pass -export or -import")
	}
}

// importCredentials reads the exported JSON on stdin and writes the keys this
// installation is missing.
//
// It writes through this installation's own HTTP API rather than into the
// SQLite file behind the running process's back. The running harness
// validates every write through the settings registry and re-syncs the ones
// that need it (internal/githubauth via httpapi.Server.OnSettingChanged), so
// a seeded App key is live without a restart; a file write would skip both.
func importCredentials(ctx context.Context, dryRun, overwrite bool) error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return fmt.Errorf("seed-credentials -import: reading stdin: %w", err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return errors.New("seed-credentials -import: nothing on stdin — pipe `harness seed-credentials -export` from the source stack into this")
	}
	var source map[string]string
	if err := json.Unmarshal(raw, &source); err != nil {
		return fmt.Errorf("seed-credentials -import: stdin is not the expected JSON: %w", err)
	}

	// This installation's own API, on its own loopback. Inside the container
	// that is the address serve binds; on the host it is whatever .env set.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	base := localBaseURL(cfg.HTTPAddr)

	alreadySet, err := credentialsSetAt(ctx, base)
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
			if err := putSetting(ctx, base, p.Key, source[p.Key]); err != nil {
				return err
			}
		}
		seeded++
	}

	if dryRun {
		fmt.Printf("dry run — nothing written. into %s:\n", base)
	} else {
		fmt.Printf("seeded %d of %d credentials into %s:\n", seeded, len(plans), base)
	}
	for _, p := range plans {
		fmt.Printf("  %-28s %s\n", p.Key, p.Action)
	}
	if seeded == 0 && !dryRun {
		fmt.Println("\nnothing to do — this installation already has every credential the source has")
	}
	return nil
}

// localBaseURL turns a bind address into a URL this process can reach itself
// on. serve binds 0.0.0.0 inside a container, which is not an address to dial
// — loopback is, and it is the same listener.
func localBaseURL(addr string) string {
	host, port, found := strings.Cut(addr, ":")
	if !found {
		return "http://127.0.0.1" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "[::]" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}
