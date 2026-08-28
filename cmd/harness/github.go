package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/mrgeoffrich/agent-harness/internal/githubapp"
	"github.com/mrgeoffrich/agent-harness/internal/workspace"
)

// The GitHub App's half of composition: the bearer token the credential
// endpoint requires, the helper command git is pointed at, the loopback
// address that helper calls back on, and the per-session gh credential a
// Bash call gets. They live here because this is where the App provider, the
// process's own listen address, and the binary's own path are all known —
// internal/githubauth and internal/tools each see only the narrow thing they
// need (ARCHITECTURE.md, docs/GITHUB-APP.md).

// generateGitCredentialToken returns the bearer token
// POST /api/github/credential requires: 32 bytes of crypto/rand, base64url,
// the same shape the run-control token takes.
//
// It is generated per process and never stored. Nothing outside this
// process's own children ever needs it — the credential helper is handed it
// through the environment it inherits — so there is nothing for a restart to
// preserve, and a value that lives only in memory is one fewer credential in
// the settings table.
//
// It is deliberately not http.control_token. Every agent session can read
// the environment it runs in, so whatever guards this endpoint is known to
// every session; that is acceptable for minting installation tokens, which a
// session's own git can do anyway, and would not be acceptable for stopping
// other people's runs.
func generateGitCredentialToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate the git credential endpoint's bearer token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// gitCredentialHelperCommand returns the command line git should run to ask
// for a credential: this binary, with the github-credential subcommand.
//
// It is resolved from os.Executable rather than a literal path so it is
// right both in the container (/usr/local/bin/harness) and on a bare-metal
// or `go run` install. An unresolvable path returns "", which makes
// githubauth.Sync fall back to the personal access token: an App with no
// helper to mint its tokens would leave git with no credential at all,
// which is worse than the credential the operator had before.
func gitCredentialHelperCommand() string {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("harness serve: cannot resolve this binary's own path, so git cannot be pointed at the GitHub App credential helper: %v", err)
		return ""
	}
	return shellQuote(exe) + " github-credential"
}

// shellQuote wraps a path for the shell git runs a "!"-prefixed credential
// helper through, so a harness installed under a path with a space in it
// still works.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// loopbackBaseURL turns the address serve binds into the URL another process
// on this machine reaches it at. The host half is deliberately discarded: a
// harness bound to 0.0.0.0, or to a container's own address, is still
// reachable at 127.0.0.1 from the children it spawns, and they are the only
// callers this is for.
func loopbackBaseURL(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return "http://127.0.0.1:" + port
}

// githubToolEnv returns the closure a session's Bash calls get their gh
// credential from (session.Runner.ToolEnv → tools.Executor.ExtraEnv).
//
// git needs nothing from this: it is pointed at the credential helper, which
// resolves a token per repository as git asks for it. gh is the one that
// does, because it reads a single GH_TOKEN and has no notion of a token per
// account. So the token it gets is the one for the account this session's
// own clones belong to (workspace.GitHubOwners), minted at the moment of
// the call so it is never a stale hour-old one.
//
// A session holding clones from more than one account gets the first
// account that yields a token, and the rest of gh's reach into the others
// will fail. That is a real limit and the honest place to state it is
// docs/GITHUB-APP.md; git remains correct for every repository either way,
// so pushing, pulling and opening a branch never depend on this choice —
// only `gh` subcommands do.
func githubToolEnv(app *githubapp.Provider) func(ctx context.Context, ws string) []string {
	// Failures are logged once per workspace. Without that, a session whose
	// repository belongs to an account the App is not installed on would log
	// the same line on every Bash call it makes for the length of the run.
	var mu sync.Mutex
	logged := make(map[string]bool)
	logOnce := func(ws string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if logged[ws] {
			return
		}
		logged[ws] = true
		log.Printf("harness serve: no GitHub App token for the repositories in %s, so gh runs unauthenticated there: %v", ws, err)
	}

	return func(ctx context.Context, ws string) []string {
		configured, err := app.Configured(ctx)
		if err != nil || !configured {
			// No App: internal/githubauth has already made the personal
			// access token ambient for the whole process, and a Bash call
			// inherits it with nothing added here.
			return nil
		}
		owners := workspace.GitHubOwners(ws)
		if len(owners) == 0 {
			return nil
		}
		var lastErr error
		for _, owner := range owners {
			token, err := app.TokenForOwner(ctx, owner)
			if err != nil {
				lastErr = err
				continue
			}
			// GITHUB_TOKEN is set alongside GH_TOKEN because gh reads either
			// and a good deal of tooling reads only the former.
			return []string{"GH_TOKEN=" + token, "GITHUB_TOKEN=" + token}
		}
		if lastErr != nil {
			logOnce(ws, lastErr)
		}
		return nil
	}
}
