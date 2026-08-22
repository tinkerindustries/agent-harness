package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The browser's start form sends skill_packs, and it must reach the queue
// request unchanged. startRunBody embeds queue.Request, so this rides through
// with no field of its own on the wire type — which is exactly why it is
// worth a test: nothing in this package mentions skill_packs, so nothing here
// would break if the field were renamed on the other side.
func TestStartRunCarriesSkillPacks(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	body := `{"prompt":"go","repos":[{"url":"https://example.com/org/app.git"}],` +
		`"permission_mode":"full","skill_packs":["unity"]}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, got)
	}
	if len(pub.requests) != 1 {
		t.Fatalf("published %d requests, want 1", len(pub.requests))
	}
	packs := pub.requests[0].SkillPacks
	if len(packs) != 1 || packs[0] != "unity" {
		t.Fatalf("published skill_packs = %v, want [unity]", packs)
	}
}

// The default is nothing at all, not an empty array that later reads as "the
// producer thought about this". A start with no skill_packs key publishes a
// request with none.
func TestStartRunDefaultsToNoSkillPacks(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	body := `{"prompt":"go","repos":[{"url":"https://example.com/org/app.git"}],"permission_mode":"full"}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 202 (body %s)", resp.StatusCode, got)
	}
	if got := pub.requests[0].SkillPacks; len(got) != 0 {
		t.Fatalf("published skill_packs = %v, want none", got)
	}
}

// An unknown pack is a 400 from the queue's own validation rather than a run
// that quietly lacks the skills somebody ticked a box for. The message names
// the offending pack, which is what a person reads off the form.
func TestStartRunRejectsUnknownSkillPack(t *testing.T) {
	pub := &fakeRunPublisher{}
	srv, _ := newStartTestServer(t, pub)

	body := `{"prompt":"go","repos":[{"url":"https://example.com/org/app.git"}],` +
		`"permission_mode":"full","skill_packs":["unitie"]}`
	resp := doWrite(t, srv, http.MethodPost, "/api/runs", body, controlAuth)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "unitie") {
		t.Errorf("400 body should name the offending pack, got %s", got)
	}
	if len(pub.requests) != 0 {
		t.Errorf("a rejected start published %d requests", len(pub.requests))
	}
}
