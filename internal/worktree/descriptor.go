package worktree

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

// DescriptorFilename is written at the root of every worktree that has run
// `harness worktree init`. It is per-worktree, per-machine state — gitignored
// alongside .env, never committed.
const DescriptorFilename = ".worktree-env.xml"

// SharedResource documents one resource this worktree does NOT get its own
// copy of, and why. It is not decoration: it is how a session working inside
// the worktree learns which of its actions reach outside it.
type SharedResource struct {
	Name   string `xml:"name,attr"`
	Impact string `xml:",chardata"`
}

// Descriptor is the machine-readable contract written at the worktree root.
// Everything else in the repo that needs a per-worktree value should read
// this file rather than hardcode one — see docs/WORKTREES.md.
type Descriptor struct {
	XMLName xml.Name `xml:"worktree-env"`

	Identity struct {
		Slug      string `xml:"slug"`
		Path      string `xml:"path"`
		Slot      int    `xml:"slot"`
		Branch    string `xml:"branch,omitempty"`
		CreatedAt string `xml:"created-at"`
	} `xml:"identity"`

	Compose struct {
		DevProjectName  string `xml:"dev-project-name"`
		TestProjectName string `xml:"test-project-name"`
	} `xml:"compose"`

	Ports Ports `xml:"ports"`

	Shared []SharedResource `xml:"shared>resource"`
}

// sharedResources is fixed across every worktree: the things this package
// deliberately does not isolate, and the one thing it refuses to touch.
// Keep this in sync with the table in docs/WORKTREES.md.
func sharedResources() []SharedResource {
	return []SharedResource{
		{Name: "host docker socket", Impact: "mounted into every harness container regardless of worktree; a session in full permission mode controls the one host daemon — see CLAUDE.md's docker socket rule"},
		{Name: "deepseek-harness-prod stack", Impact: "fixed ports 8180/4522/8522, never allocated to a worktree and never touched by this tool"},
		{Name: "GITHUB_TOKEN / DeepSeek API key", Impact: "copied into this worktree's .env from the main checkout at init time; same account, safe to use concurrently"},
		{Name: "go module cache, npm cache", Impact: "content-addressed and read-mostly; shared on purpose"},
	}
}

// NewDescriptor builds the descriptor for one worktree's allocation.
func NewDescriptor(slug, path, branch, createdAt string, slot int, ports Ports) Descriptor {
	var d Descriptor
	d.Identity.Slug = slug
	d.Identity.Path = path
	d.Identity.Slot = slot
	d.Identity.Branch = branch
	d.Identity.CreatedAt = createdAt
	d.Compose.DevProjectName = fmt.Sprintf("deepseek-harness-%s", slug)
	// Matches scripts/test.sh's own derivation: ${COMPOSE_PROJECT_NAME}-test.
	// Keep these in sync — `harness worktree rm` tears down by this exact
	// project name, found by docker label, not by recomputing it differently.
	d.Compose.TestProjectName = fmt.Sprintf("%s-test", d.Compose.DevProjectName)
	d.Ports = ports
	d.Shared = sharedResources()
	return d
}

// WriteDescriptor writes d to <worktreeRoot>/DescriptorFilename, atomically.
func WriteDescriptor(worktreeRoot string, d Descriptor) error {
	data, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encode descriptor: %w", err)
	}
	data = append([]byte(xml.Header), data...)
	data = append(data, '\n')

	path := filepath.Join(worktreeRoot, DescriptorFilename)
	tmp, err := os.CreateTemp(worktreeRoot, ".worktree-env-*.xml.tmp")
	if err != nil {
		return fmt.Errorf("create temp descriptor file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp descriptor file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp descriptor file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp descriptor file: %w", err)
	}
	return nil
}

// ReadDescriptor reads the descriptor from worktreeRoot. A missing file is
// reported as an error — inside a worktree that has run `init`, a missing
// descriptor is drift, not an absent-by-design default (see
// stack-recipes.md's "fail loudly on a missing descriptor inside a
// worktree").
func ReadDescriptor(worktreeRoot string) (Descriptor, error) {
	path := filepath.Join(worktreeRoot, DescriptorFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, err
	}
	var d Descriptor
	if err := xml.Unmarshal(data, &d); err != nil {
		return Descriptor{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return d, nil
}
