package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// The transcript screen's load path: one snapshot fetch for the backlog, then
// a stream opened at the cursor it returned. The property every test here
// circles is that the pair loses nothing — the seam is a seq number and the
// two halves are split on it, so an event committed in the window between
// the two requests is in the second half rather than in neither.

// imageDataURI is a base64 PNG data URI in the shape internal/tools writes
// into a ToolResultPayload for a vision provider.
func imageDataURI(t *testing.T) (uri string, want []byte) {
	t.Helper()
	// A real PNG, so a serving test asserts on bytes a browser would decode.
	want = writePNG(t, t.TempDir()+"/one.png")
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(want), want
}

func getSnapshot(t *testing.T, base, sessionID string) sessionSnapshot {
	t.Helper()
	resp, err := http.Get(base + "/api/sessions/" + sessionID + "/snapshot")
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status %d, want 200", resp.StatusCode)
	}
	var snap sessionSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return snap
}

func TestSnapshotCarriesTheWholeLogAndTheCursorItEndsOn(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 1}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "a"}},
		{Kind: store.KindTurnFinished, Payload: store.TurnFinishedPayload{FinishReason: "stop"}},
	})

	snap := getSnapshot(t, srv.URL, "sess-1")
	if len(snap.Events) != 3 {
		t.Fatalf("snapshot carried %d events, want 3", len(snap.Events))
	}
	if snap.Cursor != 3 {
		t.Fatalf("cursor %d, want the last event's seq (3)", snap.Cursor)
	}
	if snap.Session.ID != "sess-1" {
		t.Fatalf("snapshot session id %q, want sess-1", snap.Session.ID)
	}
}

// A session with nothing in it must still hand back a usable cursor. 0 is the
// right one: seq numbering starts at 1, so ?from=0 asks for everything, which
// is exactly what a client holding no events wants.
func TestSnapshotOfAnEmptyLogReturnsCursorZero(t *testing.T) {
	srv, st, _ := newTestServer(t)
	mustCreateSession(t, st, "sess-empty", time.Now())

	snap := getSnapshot(t, srv.URL, "sess-empty")
	if len(snap.Events) != 0 {
		t.Fatalf("snapshot carried %d events, want none", len(snap.Events))
	}
	if snap.Cursor != 0 {
		t.Fatalf("cursor %d, want 0", snap.Cursor)
	}
}

func TestSnapshotOfAMissingSessionIs404(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/sessions/sess-nope/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

// The whole point of the pair. An event committed after the snapshot was read
// but before the stream was opened belongs to neither request's obvious half,
// and it must still arrive: the stream replays from the snapshot's cursor, so
// it lands in that replay.
func TestSnapshotThenStreamDoesNotDropAnEventCommittedInBetween(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 1}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "a"}},
	})

	snap := getSnapshot(t, srv.URL, "sess-1")
	if snap.Cursor != 2 {
		t.Fatalf("cursor %d, want 2", snap.Cursor)
	}

	// The window: the browser has the snapshot and has not opened its stream.
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "b"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/sessions/sess-1/stream?from=%d", srv.URL, snap.Cursor), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	frame, err := newSSEReader(resp.Body).next()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if frame.id != "3" {
		t.Fatalf("first frame after the snapshot was seq %s, want 3 — the event committed in the window", frame.id)
	}
}

// ?from= replaces the replay, it does not merely trim it: a client that
// already holds the backlog must not be sent the backlog again, or the
// endpoint has bought nothing.
func TestStreamFromCursorReplaysOnlyWhatFollowsIt(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 1}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "a"}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "b"}},
		{Kind: store.KindRunFinished, Payload: store.RunFinishedPayload{Status: "ok"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/sess-1/stream?from=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sr := newSSEReader(resp.Body)
	var ids []string
	for i := 0; i < 2; i++ {
		frame, err := sr.next()
		if err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		ids = append(ids, frame.id)
	}
	if ids[0] != "3" || ids[1] != "4" {
		t.Fatalf("replayed %v, want [3 4] — seq 1 and 2 are the caller's already", ids)
	}
}

// Last-Event-ID is the browser's own cursor, set on an automatic reconnect,
// and it is strictly fresher than a ?from= fixed when the EventSource was
// built. Honouring the URL over the header would re-deliver everything the
// dropped connection had already handed over.
func TestStreamPrefersLastEventIDOverTheFromQuery(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: 1}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "a"}},
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "b"}},
		{Kind: store.KindRunFinished, Payload: store.RunFinishedPayload{Status: "ok"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/sess-1/stream?from=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	frame, err := newSSEReader(resp.Body).next()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if frame.id != "4" {
		t.Fatalf("first frame seq %s, want 4 — Last-Event-ID (3) should win over ?from=1", frame.id)
	}
}

// --- the image split ---

// The bytes must not be on the wire, and what replaces them must be a URL
// that gets them back. Asserting both together is the point: either alone
// would be a transcript that loads fast and shows nothing, or one that shows
// everything and loads slowly.
func TestSnapshotDetachesImageBytesToAFetchableURL(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-img", time.Now())
	uri, wantBytes := imageDataURI(t)
	appendAndPublish(t, st, h, "sess-img", []store.EventInput{
		{Kind: store.KindToolResult, Payload: store.ToolResultPayload{
			ToolCallID: "call_1", Name: "Read", Content: "Image: shot.png", ImageURL: uri,
		}},
	})

	snap := getSnapshot(t, srv.URL, "sess-img")
	if len(snap.Events) != 1 {
		t.Fatalf("snapshot carried %d events, want 1", len(snap.Events))
	}
	var payload map[string]any
	if err := json.Unmarshal(snap.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, ok := payload["image_url"]; ok {
		t.Fatal("snapshot still carries image_url; the bytes are what this endpoint exists to leave behind")
	}
	// The fields around the image are untouched — the rewrite drops one key
	// and adds one, it does not re-marshal what this package knows about.
	if payload["content"] != "Image: shot.png" || payload["name"] != "Read" {
		t.Fatalf("rewrite disturbed the rest of the payload: %v", payload)
	}
	href, ok := payload["image_href"].(string)
	if !ok {
		t.Fatalf("no image_href in payload: %v", payload)
	}
	if want := "/api/sessions/sess-img/events/1/image"; href != want {
		t.Fatalf("image_href %q, want %q", href, want)
	}

	resp, err := http.Get(srv.URL + href)
	if err != nil {
		t.Fatalf("fetch image: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("image status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type %q, want image/png", ct)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Fatalf("served %d bytes, want the %d the event carried", len(got), len(wantBytes))
	}
}

// The transcript stream carries the same projection as the snapshot. If the
// two disagreed, an image would render on a page load and vanish on a
// reconnect, or the other way round.
func TestStreamDetachesImageBytesTheSameWayTheSnapshotDoes(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-img", time.Now())
	uri, _ := imageDataURI(t)
	appendAndPublish(t, st, h, "sess-img", []store.EventInput{
		{Kind: store.KindToolResult, Payload: store.ToolResultPayload{
			ToolCallID: "call_1", Name: "Read", Content: "Image: shot.png", ImageURL: uri,
		}},
		{Kind: store.KindRunFinished, Payload: store.RunFinishedPayload{Status: "ok"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/sess-img/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	frame, err := newSSEReader(resp.Body).next()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if bytes.Contains([]byte(frame.data), []byte("image_url")) {
		t.Fatal("stream frame still carries image_url")
	}
	if !bytes.Contains([]byte(frame.data), []byte("/api/sessions/sess-img/events/1/image")) {
		t.Fatalf("stream frame carries no image_href: %s", frame.data)
	}
}

// An event row never changes, so its image never changes. Saying so is what
// makes a reload of a long transcript free rather than a re-download, and it
// is the one real difference from the workspace screenshots next door.
func TestEventImageIsCacheableForeverAndRevalidatesByETag(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-img", time.Now())
	uri, _ := imageDataURI(t)
	appendAndPublish(t, st, h, "sess-img", []store.EventInput{
		{Kind: store.KindToolResult, Payload: store.ToolResultPayload{
			ToolCallID: "call_1", Name: "Read", Content: "Image: shot.png", ImageURL: uri,
		}},
	})

	resp, err := http.Get(srv.URL + "/api/sessions/sess-img/events/1/image")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control %q, want an immutable private entry", cc)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag; a cache with an expired entry would have to re-download the bytes")
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/sessions/sess-img/events/1/image", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation status %d, want 304", resp2.StatusCode)
	}
}

// Every way of not having an image is the same 404, so walking seq numbers
// tells a caller nothing the transcript did not already say.
func TestEventImageIs404ForEverythingWithoutOne(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-img", time.Now())
	appendAndPublish(t, st, h, "sess-img", []store.EventInput{
		{Kind: store.KindToolResult, Payload: store.ToolResultPayload{
			ToolCallID: "call_1", Name: "Bash", Content: "no picture here",
		}},
	})

	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{"a tool result with no image", "/api/sessions/sess-img/events/1/image", http.StatusNotFound},
		{"a seq past the end of the log", "/api/sessions/sess-img/events/9/image", http.StatusNotFound},
		{"a session that does not exist", "/api/sessions/sess-nope/events/1/image", http.StatusNotFound},
		{"a seq that is not a sequence number", "/api/sessions/sess-img/events/0/image", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// A transcript is prose, code and command output, which is the most
// compressible thing the harness holds — and this is the one response big
// enough for it to matter.
func TestSnapshotCompressesWhenTheClientAsksForIt(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	inputs := make([]store.EventInput, 0, 40)
	for i := 0; i < 40; i++ {
		inputs = append(inputs, store.EventInput{
			Kind:    store.KindContentDelta,
			Payload: store.ContentDeltaPayload{Text: "the same highly compressible sentence, again and again. "},
		})
	}
	appendAndPublish(t, st, h, "sess-1", inputs)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/sessions/sess-1/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Go's transport adds and transparently strips gzip on its own unless the
	// header is set by hand, which would hide the thing under test.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding %q, want gzip", enc)
	}
	if v := resp.Header.Get("Vary"); v != "Accept-Encoding" {
		t.Fatalf("Vary %q, want Accept-Encoding — a shared cache must not hand gzip to a client that cannot read it", v)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	var snap sessionSnapshot
	if err := json.NewDecoder(gz).Decode(&snap); err != nil {
		t.Fatalf("decode compressed snapshot: %v", err)
	}
	if len(snap.Events) != 40 || snap.Cursor != 40 {
		t.Fatalf("compressed snapshot carried %d events at cursor %d, want 40 at 40", len(snap.Events), snap.Cursor)
	}
}

// A client that cannot decode gzip gets plain JSON, not a body it will choke
// on. Nothing on this surface negotiates anything else, so the fallback has
// to be the identity encoding rather than a 406.
func TestSnapshotServesPlainJSONWhenGzipIsNotOffered(t *testing.T) {
	srv, st, h := newTestServer(t)
	mustCreateSession(t, st, "sess-1", time.Now())
	appendAndPublish(t, st, h, "sess-1", []store.EventInput{
		{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: "a"}},
	})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/sessions/sess-1/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding %q, want none", enc)
	}
	var snap sessionSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Cursor != 1 {
		t.Fatalf("cursor %d, want 1", snap.Cursor)
	}
}
