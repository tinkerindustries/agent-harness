package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
)

// newValidationService builds a Service whose validation-only handler paths
// can be exercised without a NATS connection: every case below is rejected
// before handleLaunch or handleCollect ever touches svc.JS, so JS is left
// nil on purpose — a nil-pointer panic here would itself be a bug (an
// argument error should never reach the network).
func newValidationService(t *testing.T, roots []string) *Service {
	t.Helper()
	return &Service{
		Cfg:      config.MCPConfig{WorkspaceRoots: roots, PermissionCeiling: "full", FlashModel: "test-flash"},
		Registry: NewRegistry(),
	}
}

func TestHandleLaunchRejectsMissingDescription(t *testing.T) {
	svc := newValidationService(t, nil)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Prompt: "do it", Workspace: "x"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing description")
	}
}

func TestHandleLaunchRejectsMissingPrompt(t *testing.T) {
	svc := newValidationService(t, nil)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Description: "task", Workspace: "x"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing prompt")
	}
}

func TestHandleLaunchRejectsUnresolvableWorkspace(t *testing.T) {
	root := t.TempDir()
	svc := newValidationService(t, []string{root})
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Description: "task", Prompt: "do it", Workspace: "nope"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a workspace name with no matching directory")
	}
}

func TestHandleLaunchRejectsBadProfile(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "ws")
	svc := newValidationService(t, []string{root})
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Description: "task", Prompt: "do it", Workspace: "ws", Profile: "ultra"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unrecognised profile")
	}
}

func TestHandleLaunchRejectsBadPermissionMode(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "ws")
	svc := newValidationService(t, []string{root})
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		Description: "task", Prompt: "do it", Workspace: "ws", PermissionMode: "root",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unrecognised permission_mode")
	}
}

func TestHandleLaunchRejectsNegativeMaxSubTurns(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "ws")
	svc := newValidationService(t, []string{root})
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		Description: "task", Prompt: "do it", Workspace: "ws", MaxSubTurns: -1,
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a negative max_sub_turns (queue.Request.Validate rejects it)")
	}
}

func TestHandleCollectRejectsMissingRequestID(t *testing.T) {
	svc := newValidationService(t, nil)
	res, _, err := svc.handleCollect(context.Background(), nil, collectInput{})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing request_id")
	}
}

func mustMkdir(t *testing.T, root, name string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}
}
