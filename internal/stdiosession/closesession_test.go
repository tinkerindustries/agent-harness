package stdiosession

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// TestCloseSessionForgetsTheConversation is the /clear primitive: the runs,
// host and wake bookkeeping go, the stored row stays resumable, and a second
// close is harmless.
func TestCloseSessionForgetsTheConversation(t *testing.T) {
	f := newFixture(t, answer("done"))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("do a thing"), &created); rerr != nil {
		t.Fatalf("create: %s", rerr.Message)
	}
	f.client.waitFor(NotifyResponseCompleted)

	sessionID := created.Response.Harness.SessionID
	if sessionID == "" {
		t.Fatal("create reported no session id")
	}
	f.srv.mu.Lock()
	f.srv.woken[sessionID] = 7
	f.srv.mu.Unlock()

	var closed map[string]any
	if rerr := f.client.call(MethodCloseSession, CloseSessionParams{SessionID: sessionID}, &closed); rerr != nil {
		t.Fatalf("harness.close_session: %s", rerr.Message)
	}

	f.srv.mu.Lock()
	for id, it := range f.srv.runs {
		if it.sessionID == sessionID {
			t.Errorf("run %s survived the close", id)
		}
	}
	if _, ok := f.srv.hosts[sessionID]; ok {
		t.Error("the closed session's tool provider survived")
	}
	if _, ok := f.srv.woken[sessionID]; ok {
		t.Error("the closed session's wake mark survived")
	}
	f.srv.mu.Unlock()

	// The row and its transcript are the resume path after a clear.
	if _, err := f.srv.opts.Store.GetSession(context.Background(), sessionID); err != nil {
		t.Errorf("the session row was removed: %v", err)
	}

	// The process forgets the run, so get cannot resolve it any more.
	if rerr := f.client.call(MethodResponsesGet, IDParams{ResponseID: created.Response.ID}, nil); rerr == nil {
		t.Error("get still resolves a closed conversation's run")
	} else if rerr.Code != CodeRunNotFound {
		t.Errorf("get after close = %d, want %d", rerr.Code, CodeRunNotFound)
	}

	// A retried /clear is harmless.
	if rerr := f.client.call(MethodCloseSession, CloseSessionParams{SessionID: sessionID}, nil); rerr != nil {
		t.Errorf("second close: %s", rerr.Message)
	}
}

// TestClearStartsANewConversation is the whole client sequence: close the
// finished conversation, then create with no continuation. The create mints a
// new session id and its request carries none of the old task.
func TestClearStartsANewConversation(t *testing.T) {
	f := newFixture(t, answer("first"), answer("second"))
	f.client.handshake(ClientCapabilities{})

	var one CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %s", rerr.Message)
	}
	f.client.waitFor(NotifyResponseCompleted)

	if rerr := f.client.call(MethodCloseSession, CloseSessionParams{SessionID: one.Response.Harness.SessionID}, nil); rerr != nil {
		t.Fatalf("close: %s", rerr.Message)
	}

	var two CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("second task"), &two); rerr != nil {
		t.Fatalf("second create: %s", rerr.Message)
	}
	f.client.waitFor(NotifyResponseCompleted)

	if two.Response.Harness.SessionID == one.Response.Harness.SessionID {
		t.Errorf("the create reused session %q", one.Response.Harness.SessionID)
	}
	f.script.mu.Lock()
	last := string(f.script.requests[len(f.script.requests)-1])
	f.script.mu.Unlock()
	if strings.Contains(last, "first task") {
		t.Error("the new conversation's request carries the cleared task")
	}
}

// TestCloseSessionRefusesARunningConversation pins the stop-then-close
// ordering: a run has to be interrupted and reach its terminal frame first.
func TestCloseSessionRefusesARunningConversation(t *testing.T) {
	f := newFixture(t, callThen("call_bash", "bash", `{"command":"sleep 5"}`))
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("run something slow")
	p.Harness.PermissionMode = "full"
	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, p, &created); rerr != nil {
		t.Fatalf("create: %s", rerr.Message)
	}
	f.client.waitFor(NotifyOutputItemAdded)
	sessionID := created.Response.Harness.SessionID

	rerr := f.client.call(MethodCloseSession, CloseSessionParams{SessionID: sessionID}, nil)
	if rerr == nil {
		t.Fatal("close accepted a running conversation")
	}
	if rerr.Code != CodeRunNotRunning {
		t.Errorf("code = %d, want %d", rerr.Code, CodeRunNotRunning)
	}

	if rerr := f.client.call(MethodResponsesCancel, IDParams{ResponseID: created.Response.ID}, nil); rerr != nil {
		t.Fatalf("cancel: %s", rerr.Message)
	}
	if rerr := f.client.call(MethodCloseSession, CloseSessionParams{SessionID: sessionID}, nil); rerr != nil {
		t.Errorf("close after cancel: %s", rerr.Message)
	}
}

// TestCloseSessionEndsItsBackgroundShells is the reason the method exists.
// A shell belongs to the session, survives the run that started it, and wakes
// the session when it exits. After a clear it must be dead, so the marker it
// would have written never appears and no wake frame follows.
func TestCloseSessionEndsItsBackgroundShells(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker.txt")
	cmd := fmt.Sprintf(`{"command":"sleep 1; echo alive > '%s'","run_in_background":true}`, marker)
	f := newFixture(t,
		callThen("call_bash", "bash", cmd),
		answer("started"),
	)
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("start a shell")
	p.Harness.PermissionMode = "full"
	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, p, &created); rerr != nil {
		t.Fatalf("create: %s", rerr.Message)
	}
	f.client.waitFor(NotifyResponseCompleted)

	sessionID := created.Response.Harness.SessionID
	f.srv.mu.Lock()
	_, held := f.srv.shells[sessionID]
	f.srv.mu.Unlock()
	if !held {
		t.Fatal("no shell registry for the session, so this test proves nothing")
	}

	if rerr := f.client.call(MethodCloseSession, CloseSessionParams{SessionID: sessionID}, nil); rerr != nil {
		t.Fatalf("close: %s", rerr.Message)
	}

	f.srv.mu.Lock()
	_, still := f.srv.shells[sessionID]
	f.srv.mu.Unlock()
	if still {
		t.Error("the closed session's shell registry survived")
	}

	// Long enough for the shell's own sleep to have finished. A wake frame or
	// the marker means the shell outlived the conversation.
	select {
	case m := <-f.client.notifications:
		t.Fatalf("a closed conversation produced %s", m.Method)
	case <-time.After(1500 * time.Millisecond):
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the closed conversation's shell kept running")
	}
}

// TestCloseSessionLeavesASuccessorItsShells pins the compaction handover.
// The successor's row owns the registry the parent started with, so closing
// the retired parent must not end the live conversation's shells.
func TestCloseSessionLeavesASuccessorItsShells(t *testing.T) {
	s := NewServer(Options{})
	shells := tools.NewShells("sess-live", nil)
	s.shells["sess-parent"] = shells
	s.shells["sess-live"] = shells
	s.runs["run_parent"] = &run{id: "run_parent", sessionID: "sess-parent"}

	if _, rerr := s.closeSession(mustJSON(CloseSessionParams{SessionID: "sess-parent"})); rerr != nil {
		t.Fatalf("close parent: %s", rerr.Message)
	}
	if _, ok := s.shells["sess-parent"]; ok {
		t.Error("the retired parent's registry key survived")
	}
	if s.shells["sess-live"] != shells {
		t.Fatal("closing the parent dropped the successor's registry")
	}

	if _, rerr := s.closeSession(mustJSON(CloseSessionParams{SessionID: "sess-live"})); rerr != nil {
		t.Fatalf("close successor: %s", rerr.Message)
	}
	if _, ok := s.shells["sess-live"]; ok {
		t.Error("the successor's registry key survived its own close")
	}
}

// TestCloseSessionRefusesTheSuccessorOfARunningCompaction pins the other half
// of the handover: a run that compacts mid-flight keeps the id it started
// under, so the running check has to follow its registry to the successor.
func TestCloseSessionRefusesTheSuccessorOfARunningCompaction(t *testing.T) {
	s := NewServer(Options{})
	shells := tools.NewShells("sess-parent", nil)
	shells.SetSessionID("sess-child")
	s.shells["sess-parent"] = shells
	s.running = &run{id: "run_live", sessionID: "sess-parent"}

	_, rerr := s.closeSession(mustJSON(CloseSessionParams{SessionID: "sess-child"}))
	if rerr == nil {
		t.Fatal("close accepted the successor of a running compaction")
	}
	if rerr.Code != CodeRunNotRunning {
		t.Errorf("code = %d, want %d", rerr.Code, CodeRunNotRunning)
	}
	if s.shells["sess-parent"] != shells {
		t.Error("the running run's registry was dropped")
	}
}

// TestCloseSessionIsAdvertisedInEveryHandshake pins the capability a client
// feature-detects /clear with.
func TestCloseSessionIsAdvertisedInEveryHandshake(t *testing.T) {
	if !NewResponses().Initialize(Handshake{}).(InitializeResult).Capabilities.CloseSession {
		t.Error("the Responses handshake does not advertise close_session")
	}
	if !NewInteractions().Initialize(Handshake{}).(iactInitializeResult).Capabilities.CloseSession {
		t.Error("the Interactions handshake does not advertise close_session")
	}
	if !NewManagedAgents().Initialize(Handshake{}).(maInitializeResult).Capabilities.CloseSession {
		t.Error("the ManagedAgents handshake does not advertise close_session")
	}
}
