package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// newValidationService builds a Service whose validation-only handler paths
// can be exercised without any wiring: every case below is rejected before
// handleLaunch or handleCollect ever touches svc.Publisher or the HTTP
// client, so both are left nil on purpose — a nil-pointer panic here would
// itself be a bug (an argument error should never reach the network).
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
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Title: "A title", Prompt: "do it", Repos: testLaunchRepos(), PermissionMode: "full"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing description")
	}
}

// TestHandleLaunchRejectsMissingTitle pins the launch path's presence rule:
// the MCP tool requires a title even though the queue layer treats it as
// optional — the run's heading is one of the two lines the harness UI
// renders for a launch.
func TestHandleLaunchRejectsMissingTitle(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Description: "a task", Prompt: "do it", Repos: testLaunchRepos(), PermissionMode: "full"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing title")
	}
	if !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "title is required") {
		t.Fatalf("expected the refusal to name the missing title, got: %s", res.Content[0].(*mcpsdk.TextContent).Text)
	}
}

// TestHandleLaunchRejectsOverLengthTitle pins the word cap at the launch
// surface: an 11-word title is refused before it can reach the queue, the
// same way a bad permission mode is.
func TestHandleLaunchRejectsOverLengthTitle(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		Title:       "one two three four five six seven eight nine ten eleven",
		Description: "a task", Prompt: "do it", Repos: testLaunchRepos(), PermissionMode: "full",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an 11-word title")
	}
}

// newQueuedHarnessService builds a Service whose publish is captured and
// whose accepted wait sees a 404 — the "the pool has not claimed it"
// signal — so a good launch's happy path runs to the queued outcome without
// a broker: the request validates, publishes, and reports "queued" because
// nothing claimed it within the wait window.
func newQueuedHarnessService(t *testing.T) (*Service, *capturePublisher) {
	t.Helper()
	svc := newValidationService(t)
	captured := &capturePublisher{}
	svc.Publisher = captured
	api := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(api.Close)
	svc.Cfg.HarnessBaseURL = api.URL
	svc.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	return svc, captured
}

// TestLaunchCarriesAllFourFieldsOntoTheRequest launches with a title,
// description, and a phase position and captures the published work request,
// asserting all four reach the wire exactly as given — the launch path is
// where a phased chain stamps its position.
func TestLaunchCarriesAllFourFieldsOntoTheRequest(t *testing.T) {
	svc, captured := newQueuedHarnessService(t)

	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		Title:          "Add session title fields",
		Description:    "Carry a title, description, and phase position from every producer onto the session row.",
		Prompt:         "do it",
		Repos:          testLaunchRepos(),
		PermissionMode: "full",
		Phase:          2,
		TotalPhases:    5,
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	if lo, ok := res.StructuredContent.(launchOutput); !ok || lo.Status != "queued" {
		t.Fatalf("expected the queued launch outcome, got %+v", res.StructuredContent)
	}

	var got queue.Request
	if err := json.Unmarshal(captured.lastPublished(), &got); err != nil {
		t.Fatalf("parse published work request: %v", err)
	}
	if got.Title != "Add session title fields" {
		t.Fatalf("published title = %q, want the submitted title", got.Title)
	}
	if got.Description != "Carry a title, description, and phase position from every producer onto the session row." {
		t.Fatalf("published description = %q, want the submitted description", got.Description)
	}
	if got.Phase != 2 || got.TotalPhases != 5 {
		t.Fatalf("published phase = %d/%d, want 2/5", got.Phase, got.TotalPhases)
	}

	// The registry entry the deepseek_runs listing reads carries the same
	// four, so the phase position is visible in the MCP surface too.
	records := svc.Registry.list()
	if len(records) != 1 {
		t.Fatalf("expected one registry record, got %d", len(records))
	}
	if records[0].Title != got.Title || records[0].Description != got.Description ||
		records[0].Phase != 2 || records[0].TotalPhases != 5 {
		t.Fatalf("registry record = %+v, want the submitted title fields", records[0])
	}
}

func TestHandleLaunchRejectsMissingPrompt(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Title: "A title", Description: "task", Repos: testLaunchRepos(), PermissionMode: "full"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing prompt")
	}
}

func TestHandleLaunchRejectsMissingRepos(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Title: "A title", Description: "task", Prompt: "do it", PermissionMode: "full"})
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
		Title: "A title", Description: "task", Prompt: "do it", PermissionMode: "full",
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
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{Title: "A title", Description: "task", Prompt: "do it", Repos: testLaunchRepos(), Profile: "ultra", PermissionMode: "full"})
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
		Title: "A title", Description: "task", Prompt: "do it", Repos: testLaunchRepos(), PermissionMode: "root",
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
		Title:          "A title", Description: "task", Prompt: "do it", Repos: testLaunchRepos(), MaxSubTurns: -1,
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a negative max_sub_turns (queue.Request.Validate rejects it)")
	}
}

// TestHandleLaunchRejectsUnknownJobType checks the provenance validation
// that runs before the publish: Publisher is nil on this service, so a
// launch that reached the publish would panic, and the error result alone
// means nothing was published.
func TestHandleLaunchRejectsUnknownJobType(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Title:          "A title", Description: "task", Prompt: "do it", Repos: testLaunchRepos(),
		JobType: "make-coffee",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unknown job_type")
	}
}

// TestHandleLaunchRejectsRetiredUserParentAgentType checks the pre-publish
// validation for the parent agent pair: the type "user" is retired — the
// producer sets parent_is_user instead — so a caller sending it gets a tool
// error even with no id.
func TestHandleLaunchRejectsRetiredUserParentAgentType(t *testing.T) {
	svc := newValidationService(t)
	res, _, err := svc.handleLaunch(context.Background(), nil, launchInput{
		PermissionMode: "full",
		Title:          "A title", Description: "task", Prompt: "do it", Repos: testLaunchRepos(),
		ParentAgentType: "user",
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for the retired parent_agent_type \"user\"")
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

// TestLaunchWritesAttachmentsToTheStore pins the attachment path of the
// deepseek_agent tool: the bytes are stored before the publish and the
// caller receives the ids on the work request, so the NATS request stays
// small and the worker can materialise the files.
func TestLaunchWritesAttachmentsToTheStore(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := newValidationService(t)
	svc.Store = st
	svc.Settings = settings.NewResolver(st)

	ids, err := svc.writeAttachments(context.Background(), []launchAttachment{
		{Name: "mockup.png", MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("mockup bytes"))},
		{Name: "dark.webp", Data: base64.StdEncoding.EncodeToString([]byte("dark bytes"))}, // mime derived from the name
	})
	if err != nil {
		t.Fatalf("writeAttachments: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d ids, want 2", len(ids))
	}

	att, err := st.GetAttachment(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if att.Name != "mockup.png" || att.MIMEType != "image/png" || string(att.Data) != "mockup bytes" {
		t.Errorf("attachment = %+v, want the stored mockup", att)
	}
	att2, err := st.GetAttachment(context.Background(), ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if att2.MIMEType != "image/webp" {
		t.Errorf("omitted mime_type should be derived from the name, got %q", att2.MIMEType)
	}
}

// TestLaunchRefusesBadAttachments pins the same refusals the browser start
// carries: a bad extension, a mismatched MIME type, a path-shaped name, and
// bytes over the cap are tool errors, and a launch that carries attachments
// against a Service with no store wired is refused rather than stripped.
func TestLaunchRefusesBadAttachments(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := newValidationService(t)
	svc.Store = st
	svc.Settings = settings.NewResolver(st)

	valid := base64.StdEncoding.EncodeToString([]byte("mockup bytes"))
	for _, tc := range []struct {
		name string
		att  launchAttachment
		want string
	}{
		{name: "bad extension", att: launchAttachment{Name: "mockup.gif", Data: valid}, want: "only PNG, JPEG, and WebP"},
		{name: "mime mismatch", att: launchAttachment{Name: "mockup.png", MIMEType: "image/webp", Data: valid}, want: "does not match"},
		{name: "path-shaped name", att: launchAttachment{Name: "../mockup.png", MIMEType: "image/png", Data: valid}, want: "plain file name"},
		{name: "not base64", att: launchAttachment{Name: "mockup.png", Data: "!!!not base64!!!"}, want: "not valid base64"},
		{name: "over the byte cap", att: launchAttachment{Name: "mockup.png", MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(make([]byte, launchAttachmentMaxBytesDefault+1))}, want: "per-file limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.writeAttachments(context.Background(), []launchAttachment{tc.att}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to carry %q", err, tc.want)
			}
		})
	}

	// A launch carrying attachments against a service with no store is an
	// ordinary tool error, not a silently stripped attachment.
	noStore := newValidationService(t)
	res, _, err := noStore.handleLaunch(context.Background(), nil, launchInput{
		Title:          "A title",
		Description:    "task",
		Prompt:         "match the mockup",
		Repos:          testLaunchRepos(),
		PermissionMode: "full",
		Attachments:    []launchAttachment{{Name: "mockup.png", MIMEType: "image/png", Data: valid}},
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "no store is wired") {
		t.Errorf("expected a no-store refusal, got: %s", res.Content[0].(*mcpsdk.TextContent).Text)
	}
}
