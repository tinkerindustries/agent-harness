package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// GitHubOwners returns the github.com account names the clones in a
// prepared session workspace belong to, lowercased, without duplicates, in
// directory order.
//
// It exists for the one credential that cannot be routed per repository the
// way git's is: `gh` reads a single GH_TOKEN and has no notion of a
// different token per account, so something has to decide which
// installation's token a session's gh calls get, and the clones the session
// is actually working in are the best evidence available
// (cmd/harness/serve.go builds that decision on top of this;
// docs/GITHUB-APP.md, "What gh gets").
//
// It reads each clone's own .git/config rather than running git, because it
// is called on the way into every Bash call and spawning a process there to
// learn something that has not changed since the clone would be paid for on
// every command. A directory that is not a clone, a clone with no
// github.com remote, and an unreadable config are all skipped: an empty
// result means "no GitHub account to name", which the caller treats as
// having no token to offer rather than as a failure.
func GitHubOwners(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var owners []string
	seen := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		config, err := os.ReadFile(filepath.Join(dir, entry.Name(), ".git", "config"))
		if err != nil {
			continue
		}
		for _, owner := range gitHubOwnersIn(string(config)) {
			if seen[owner] {
				continue
			}
			seen[owner] = true
			owners = append(owners, owner)
		}
	}
	return owners
}

// gitHubOwnersIn pulls the owner out of every github.com remote URL in one
// .git/config. Both spellings a clone can carry are handled — the HTTPS URL
// the harness clones with, and the SSH one a repository configured by hand
// may have — because the owner is what is wanted and neither form's
// credentials are.
func gitHubOwnersIn(config string) []string {
	var owners []string
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		if owner := gitHubOwnerOf(strings.TrimSpace(value)); owner != "" {
			owners = append(owners, owner)
		}
	}
	return owners
}

// gitHubOwnerOf returns the account segment of a github.com remote URL, or
// "" for a URL pointing anywhere else.
func gitHubOwnerOf(remote string) string {
	rest := ""
	switch {
	case strings.HasPrefix(remote, "https://github.com/"):
		rest = strings.TrimPrefix(remote, "https://github.com/")
	case strings.HasPrefix(remote, "http://github.com/"):
		rest = strings.TrimPrefix(remote, "http://github.com/")
	case strings.HasPrefix(remote, "ssh://git@github.com/"):
		rest = strings.TrimPrefix(remote, "ssh://git@github.com/")
	case strings.HasPrefix(remote, "git@github.com:"):
		rest = strings.TrimPrefix(remote, "git@github.com:")
	default:
		// A URL carrying a userinfo section (https://x-access-token:...@github.com/owner/repo)
		// is what a credential written into a remote looks like; the owner is
		// still the first segment after the host.
		if i := strings.Index(remote, "@github.com/"); i >= 0 && strings.HasPrefix(remote, "http") {
			rest = remote[i+len("@github.com/"):]
		} else {
			return ""
		}
	}
	owner, _, ok := strings.Cut(rest, "/")
	if !ok || owner == "" {
		return ""
	}
	return strings.ToLower(owner)
}
