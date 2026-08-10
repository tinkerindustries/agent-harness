package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is one worktree's registered allocation. Keyed in Registry by Slug —
// not by branch, which gets renamed and deleted — and keyed by directory
// name, which is what a person actually types.
type Entry struct {
	Slug              string `json:"slug"`
	Path              string `json:"path"`
	Slot              int    `json:"slot"`
	Branch            string `json:"branch,omitempty"`
	ComposeProjectDev string `json:"compose_project_dev"`
	ComposeProjectTst string `json:"compose_project_test"`
	Ports             Ports  `json:"ports"`
	CreatedAt         string `json:"created_at"`
}

// Registry is the host-global record of every worktree that has run
// `harness worktree init`. It lives outside every worktree — see
// docs/WORKTREES.md — because its whole job is coordinating across them.
type Registry struct {
	Version   int              `json:"version"`
	Worktrees map[string]Entry `json:"worktrees"`
}

// registryDir is host-global, not repo-local: a linked worktree cannot see
// files that live in a sibling worktree, so the registry has to live
// somewhere every worktree can reach.
func registryDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".deepseek-harness"), nil
}

func registryPath() (string, error) {
	dir, err := registryDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worktrees.json"), nil
}

func lockPath() (string, error) {
	dir, err := registryDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worktrees.lock"), nil
}

// staleLockAge is how long an exclusive-create lockfile can stand before a
// later caller assumes the process that made it died without cleaning up
// and removes it. Registry read-modify-write cycles are a handful of
// filesystem calls, so anything still held this long is stale, not slow.
const staleLockAge = 15 * time.Second

// WithLock runs fn holding an exclusive-create lockfile around the
// registry's read-modify-write cycle, so two `harness worktree init` runs
// launched at once cannot both read "slot 3 free" and both take it. Every
// caller that loads the registry, changes it, and saves it must do so
// inside fn.
func WithLock(fn func() error) error {
	dir, err := registryDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	lp, err := lockPath()
	if err != nil {
		return err
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create lock %s: %w", lp, err)
		}
		if info, statErr := os.Stat(lp); statErr == nil && time.Since(info.ModTime()) > staleLockAge {
			os.Remove(lp) // stale: the process that made it is gone
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for lock %s (another `harness worktree` command may be running)", lp)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.Remove(lp)

	return fn()
}

// LoadRegistry reads the registry, returning an empty one (not an error) if
// it has never been written. Safe to call read-only (list, doctor); callers
// that will modify and save it back must do so from inside WithLock.
func LoadRegistry() (*Registry, error) {
	path, err := registryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{Version: 1, Worktrees: map[string]Entry{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if reg.Worktrees == nil {
		reg.Worktrees = map[string]Entry{}
	}
	return &reg, nil
}

// Save writes the registry atomically: serialise to a temp file in the same
// directory, then rename over the target, so a crash mid-write cannot leave
// a truncated registry behind.
func (r *Registry) Save() error {
	path, err := registryPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "worktrees-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp registry file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp registry file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp registry file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp registry file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp registry file: %w", err)
	}
	return nil
}

// usedSlots returns the set of slots already taken, for AllocateSlot.
func (r *Registry) usedSlots() map[int]bool {
	used := make(map[int]bool, len(r.Worktrees))
	for _, e := range r.Worktrees {
		used[e.Slot] = true
	}
	return used
}

// AllocateSlot returns the lowest free slot in 1..MaxSlots whose ports are
// actually free right now, skipping any that are registered or that fail a
// live bind probe (something outside the registry may hold it). It does not
// mutate the registry or bind anything long-term — the caller commits by
// writing an Entry and saving. Call from inside WithLock.
func AllocateSlot(r *Registry) (int, Ports, error) {
	used := r.usedSlots()
	for slot := 1; slot <= MaxSlots; slot++ {
		if used[slot] {
			continue
		}
		ports := PortsForSlot(slot)
		if err := ports.probeFree(); err != nil {
			continue
		}
		return slot, ports, nil
	}
	return 0, Ports{}, fmt.Errorf("no free slot in 1..%d — every port band is exhausted or held by something outside the registry; run `harness worktree doctor` to check for stale entries", MaxSlots)
}
