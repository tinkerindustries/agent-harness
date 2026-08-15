// Package worktree allocates per-worktree ports and compose project names so
// sibling git worktrees of this repo can run docker compose at the same time
// without colliding. See docs/WORKTREES.md.
package worktree

import (
	"fmt"
	"regexp"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateSlug enforces the kebab-case rule the registry, the compose
// project names, and the descriptor filename all assume.
func ValidateSlug(s string) error {
	if !slugPattern.MatchString(s) {
		return fmt.Errorf("slug must be kebab-case (lowercase letters, digits, hyphens, starting with a letter or digit): %q", s)
	}
	return nil
}
