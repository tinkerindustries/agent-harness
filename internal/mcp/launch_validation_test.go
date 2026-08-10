package mcp

import (
	"context"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
)

// newValidationService builds a Service whose validation-only handler paths
// can be exercised without a NATS connection: every case below is rejected
// before handleLaunch or handleCollect ever touches svc.JS, so JS is left
// nil on purpose — a nil-pointer panic here would itself be a bug (an
// argument error should never reach the network).
func newValidationService(t *testing.T) *Service {
	t.Helper()
	return &Service{
		Cfg:      config.MCPConfig{PermissionCeiling: "full", FlashModel: "test-flash"},
		Registry: NewRegistry(),
	}
}

// testLaunchRepos is one valid repository, the minimum a launch needs to
// get past validation.
func testLaunchRepos() []launchRepo {
	return []launchRepo{{URL: "https://example.com/org/app.git"}}
}

func TestHandleLaunchRejectsMissingDescription(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Prompt: "do it", Repos: testLaunchRepos(), PermissionMode: "full"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing description")
	}
}

func TestHandleLaunchRejectsMissingPrompt(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Description: "task", Repos: testLaunchRepos(), PermissionMode: "full"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing prompt")
	}
}

func TestHandleLaunchRejectsMissingRepos(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Description: "task", Prompt: "do it", PermissionMode: "full"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a launch naming no repositories")
	}
}

func TestHandleLaunchRejectsUnsupportedRepoURL(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		Description: "task", Prompt: "do it", PermissionMode: "full",
		Repos: []launchRepo{{URL: "ext::sh -c 'touch /tmp/pwned'"}},
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a repo url git would run as a command")
	}
}

func TestHandleLaunchRejectsBadProfile(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Description: "task", Prompt: "do it", Repos: testLaunchRepos(), Profile: "ultra", PermissionMode: "full"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unrecognised profile")
	}
}

func TestHandleLaunchRejectsBadPermissionMode(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		Description: "task", Prompt: "do it", Repos: testLaunchRepos(), PermissionMode: "root",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unrecognised permission_mode")
	}
}

func TestHandleLaunchRejectsNegativeMaxSubTurns(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Description:    "task", Prompt: "do it", Repos: testLaunchRepos(), MaxSubTurns: -1,
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a negative max_sub_turns (queue.Request.Validate rejects it)")
	}
}

// TestHandleLaunchRejectsUnknownJobType checks the provenance validation
// that runs before the publish: JS is nil on this service, so a launch that
// reached the network would panic, and the error result alone means nothing
// was published.
func TestHandleLaunchRejectsUnknownJobType(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Description:    "task", Prompt: "do it", Repos: testLaunchRepos(),
		JobType: "make-coffee",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unknown job_type")
	}
}

// TestHandleLaunchRejectsUserParentAgentWithID checks the same pre-publish
// validation for the parent agent pair: the reserved type "user" must not
// carry an id, because there is no agent session to trace back to.
func TestHandleLaunchRejectsUserParentAgentWithID(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Description:    "task", Prompt: "do it", Repos: testLaunchRepos(),
		ParentAgentType: "user",
		ParentAgentID:   "sess-1",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a user parent agent carrying an id")
	}
}

func TestHandleCollectRejectsMissingRequestID(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleCollect(context.Background(), nil, collectInput{})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing request_id")
	}
}
