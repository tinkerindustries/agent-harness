package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/attachment"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// The three actions that touch a run (docs/RUN-CONTROL.md): stop, through
// the declared RunController seam; steer, a plain store write the loop
// reads at its next sub-turn boundary; and start, which publishes a
// validated work request through the declared RunPublisher seam and, on the
// way there, validates and stores any attachments the request carries.

// --- run control: stop ---

// stopSessionBody is the JSON body POST /api/sessions/{id}/stop accepts: the
// operator's reason, optional, carried verbatim as the message of the
// cancelled result (docs/RUN-CONTROL.md "The HTTP surface").
type stopSessionBody struct {
	Reason string `json:"reason"`
}

// handleStopSession serves POST /api/sessions/{id}/stop: asks this process's
// worker pool to end the run, whatever state it is in — a healthy run ends at
// its next check point, a wedged one is force-finished after the grace period
// (docs/RUN-CONTROL.md "Stopping"). The whole path is non-blocking: the 202 is
// the acceptance, and the terminal state arrives over the session's own SSE
// stream.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; the bearer token (401 missing or wrong, 503 when no
// token is configured — a missing credential fails closed); the session
// existing in the store (404); this process running it (409 naming the
// session's actual status — the honest answer for a session that already
// finished, and for one being run by nothing at all); and 202
// {"session_id", "stopping": true} otherwise. A second stop for a run already
// stopping is another 202, not a 409: stopping is idempotent
// (docs/RUN-CONTROL.md "The HTTP surface").
//
// No If-Match, deliberately: that rule (docs/DATA-API.md "Optimistic
// concurrency") governs mutations of a row — it stops an operator's write
// landing on a row that changed since they read it. A stop is an action on a
// run, not an edit of a row, and a running session's version changes
// continuously underneath the caller, so requiring a version echo would make
// a correct stop racy by construction.
func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	var body stopSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"reason": "..."}`})
		return
	}
	// The registry is asked before the store, and the order is load-bearing. A
	// run is registered before its workspace is prepared, and its session row
	// is not created until the session loop starts — so a run wedged in a
	// git clone is registered, stoppable, and has no row to look up. Asking
	// the store first would answer 404 for exactly the run an operator most
	// needs to end (docs/RUN-CONTROL.md "Half two": the escalation has its own
	// branch for a stop that finds no session row to mark).
	//
	// The controller is nil in any caller that has no pool; that caller is
	// running nothing, and every session falls through to the store below.
	id := r.PathValue("id")
	if s.Run != nil && s.Run.Running(id) {
		if err := s.Run.Stop(id, body.Reason); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"session_id": id, "stopping": true})
		return
	}

	// Not running here: the store decides whether that is a 404 for a session
	// nobody has heard of, or a 409 for one this process finished or never
	// ran, naming the status it actually holds.
	sess, err := s.Store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": fmt.Sprintf("session %s is not running in this process (status %s)", sess.ID, sess.Status),
	})
}

// steerSessionBody is the JSON body POST /api/sessions/{id}/steer accepts:
// the operator's instruction, verbatim, plus an optional source naming where
// it came from — "web", "mcp", or "cli" — which defaults to "web" when
// absent (the browser is the HTTP surface's primary client). The text is
// carried verbatim into the user message the loop folds, so a caller's
// formatting survives (docs/RUN-CONTROL.md "Steering").
type steerSessionBody struct {
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}

// handleSteerSession serves POST /api/sessions/{id}/steer: appends a
// steer_message event to the session's log for the loop to pick up at its
// next sub-turn boundary (docs/RUN-CONTROL.md "Steering"). The loop reads the
// store once per sub-turn and never blocks on it, so this handler does not
// touch the RunController the stop handler needs — a reader who just read
// that handler will expect one, and it is deliberately absent: steering is a
// store write by the handler and a store read by the loop, with the
// database — which every replica shares — as the seam. The event is fanned
// out to the session's SSE stream so the transcript shows it immediately as
// a pending steer block.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; the bearer token (401 missing or wrong, 503 when no
// token is configured); a body whose text is empty or whitespace only (400 —
// an empty steer would be a user message with nothing to say); the session
// existing in the store (404); the session being `running` (409 — a steer for
// a finished run would sit in the log forever, unapplied and unexplained);
// and 202 {"session_id", "seq"} otherwise, where seq is the sequence number
// the steer_message landed at. Unlike stop, steering is deliberately NOT
// idempotent: two steers are two instructions, which is why the response
// carries the seq the caller's text landed at.
//
// No If-Match, for the same reason as stop: this is an action on a run, not
// an edit of a row, and a running session's version changes continuously
// underneath the caller (docs/RUN-CONTROL.md "The HTTP surface").
func (s *Server) handleSteerSession(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	var body steerSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"text": "..."}`})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text must not be empty"})
		return
	}
	id := r.PathValue("id")
	sess, err := s.Store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	if sess.Status != store.StatusRunning {
		// A steer keeps refusing anything but "running" — a creating session
		// has no loop to read the steer — but the message names the actual
		// situation rather than reading as though the run is over.
		message := fmt.Sprintf("session %s is not running (status %s); a steer needs a running run", sess.ID, sess.Status)
		if sess.Status == store.StatusCreating {
			message = fmt.Sprintf("session %s is still being prepared (status %s); a steer needs the workspace ready", sess.ID, sess.Status)
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": message,
		})
		return
	}

	source := body.Source
	if source == "" {
		source = "web"
	}
	appended, err := s.Store.AppendEvents(r.Context(), id, []store.EventInput{{
		Kind:    store.KindSteerMessage,
		Payload: store.SteerMessagePayload{Text: body.Text, Source: source},
	}})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Fan the event out to the session's transcript stream the way the runner
	// fans out its own commits, so the browser's steer block appears the
	// moment the steer is accepted rather than at the next sub-turn.
	s.Hub.PublishEvents(id, appended)
	writeJSON(w, http.StatusAccepted, map[string]any{"session_id": id, "seq": appended[0].Seq})
}

// --- run control: continue ---

// resumeSessionBody is the JSON body POST /api/sessions/{id}/resume accepts:
// the person's next message, carried verbatim into the user message the loop
// folds, exactly as steer's text is. There is no source field — a resume is
// the browser's own verb (docs/RUN-CONTROL.md "Continuing").
type resumeSessionBody struct {
	Text string `json:"text"`
}

// handleResumeSession serves POST /api/sessions/{id}/resume: continues a
// session that already reached a terminal status, so a person who started a
// run in the browser can keep talking to it instead of starting a fresh run
// against a fresh clone (docs/RUN-CONTROL.md "Continuing").
//
// It publishes rather than reaching into the loop, which is why this handler
// sits beside handleStartRun and not beside handleStopSession. Starting is
// already a publish, and a resume is the same act naming a session instead of
// naming repositories: the request goes onto the same durable queue, gets
// claimed by whichever worker has a slot, and inherits claim, heartbeat,
// redelivery, the stop registry and its own work_requests result row. No
// second code path ever starts a loop, and nothing here needs the process
// that ran the session originally to be the one that continues it.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; the bearer token (401 missing or wrong, 503 when no
// token is configured); a publisher wired in (503, the shape the start
// handler uses); a body whose text is empty or whitespace only (400); the
// session existing in the store (404); and the three refusals Runner.Resume
// itself makes, stated here so a caller hears them now rather than a worker
// discovering them later (409) — a live session, which wants steer instead,
// and a session retired by compaction, whose continuation is its child.
// Otherwise 202 {"session_id", "request_id"}.
//
// No If-Match, for the reason stop and steer already give: this is an action
// on a run, not an edit of a row.
func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	if s.Publisher == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "run control is not configured: no run publisher is wired (start harness serve once)",
		})
		return
	}
	var body resumeSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"text": "..."}`})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text must not be empty"})
		return
	}
	id := r.PathValue("id")
	sess, err := s.Store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	switch {
	case store.IsLive(sess.Status):
		// The live half of the same composer: a running session reads a new
		// message at its next sub-turn boundary, and a creating one is not
		// ready for either verb yet. Naming steer keeps the two endpoints
		// legible as one pair rather than two overlapping ones.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("session %s is still live (status %s); steer it instead of resuming it", sess.ID, sess.Status),
		})
		return
	case sess.Status == store.StatusCompacted:
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("session %s was retired by compaction; resume its child session instead", sess.ID),
		})
		return
	}

	// Permission mode and model are copied off the frozen session row, not
	// taken from the caller. They are what the resumed run will actually use
	// — Runner.Resume reads both back off the same row, because a resumed
	// session's prefix cannot change (docs/CACHE.md) — and carrying them on
	// the request is what lets it satisfy queue.Request.Validate unchanged
	// instead of the queue having to relax its rules for this producer.
	req := queue.Request{
		RequestID:       randomRequestID(),
		ResumeSessionID: sess.ID,
		Prompt:          body.Text,
		Model:           sess.Model,
		PermissionMode:  sess.PermissionMode,
		JobType:         sess.JobType,
		ParentIsUser:    true,
		ParentAgentID:   s.operatorName(r.Context()),
	}
	if err := req.Validate(); err != nil {
		// A 409 rather than a 400: nothing the caller sent is wrong. Every
		// field this request carries beyond the message came off the session
		// row, so a validation failure here means the row itself holds
		// something this build will not run — a permission mode retired since
		// the session was created, say. The queue's own message says which,
		// and naming the session is what stops it reading as a complaint
		// about the message somebody just typed.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("session %s cannot be continued: its stored configuration is not one this build can run (%v)", sess.ID, err),
		})
		return
	}
	if err := s.Publisher.PublishRequest(r.Context(), req); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"session_id": sess.ID, "request_id": req.RequestID})
}

// --- run control: start ---

// handleStartRun serves POST /api/runs: accepts a work request in the
// queue's own wire shape and enqueues it through the RunPublisher seam,
// making the browser one more producer among the existing ones — harness
// publish and deepseek_agent — so the claim/heartbeat/redelivery machinery
// stays the only way a session ever starts, with no second code path to
// keep in sync (docs/RUN-CONTROL.md "Starting is a publish, so the seam is
// a publisher"). The 202 is an acceptance, not an outcome: the session
// appears on the existing GET /api/stream list feed once the pool claims
// the request, and this handler never waits for, or answers with, the run's
// result.
//
// The guards and preconditions, in order: the content-type and origin guards
// every write carries; the bearer token (401 missing or wrong, 503 when no
// token is configured); a publisher wired in (503 — a Server built without
// one cannot start anything, and a missing capability must fail closed, the
// same shape the missing token has); a body that decodes to queue.Request
// (400); validation by queue.Request.Validate — the queue's own rules, so a
// body accepted here can never drift from what the worker checks (400
// carrying the validator's message); and 202 {"request_id": "..."} once the
// publish lands. request_id is optional on this surface and generated when
// absent — a browser form has no idempotency key to offer — and a caller
// that supplies one gets the same deduplication every other producer gets.
//
// The body may also carry an attachments array (name, mime_type, base64
// data). Each attachment is validated — plain file name, PNG/JPEG/WebP
// extension, bytes within the per-file cap, count within the cap
// (tools.attachments_max_count, tools.attachments_max_bytes) — and written
// to the store before validation, so the published request carries only the
// attachment ids and never the bytes (docs/DATA-API.md).
//
// The handler overwrites the three provenance fields — parent_is_user,
// parent_agent_type, parent_agent_id — before validation: a person started
// this run directly, so parent_is_user is true and the parent agent fields
// are empty (parent_agent_id carries the identity.operator name when one is
// configured). The overwrite is what makes parent_is_user trustworthy,
// because a caller cannot assert its own provenance on this endpoint; the
// value it sent is discarded either way, never answered with a 400 that
// would force the frontend to carry a field it must not send (D5, D7).
//
// No If-Match, for the same reason as stop and steer: this is not a mutation
// of a row, and a work request has no row to mutate yet
// (docs/RUN-CONTROL.md "The HTTP surface").
func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	if s.Publisher == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "run control is not configured: no run publisher is wired (start harness serve once)",
		})
		return
	}
	var body startRunBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<26)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: expected a work request (prompt, repos, permission_mode, ...)"})
		return
	}
	req := body.Request
	if req.RequestID == "" {
		req.RequestID = randomRequestID()
	}
	req.ParentIsUser = true
	req.ParentAgentType = ""
	req.ParentAgentID = s.operatorName(r.Context())
	// Attachments are written to the store before validation, so a request
	// that passes Validate is already complete: the bytes never ride the
	// queue request (one request is one row of the work_queue table, and a
	// multi-megabyte payload would bloat it), only the ids do
	// (docs/DATA-API.md).
	ids, err := s.writeAttachments(r.Context(), body.Attachments)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.AttachmentIDs = ids
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.Publisher.PublishRequest(r.Context(), req); err != nil {
		// The one error path this handler owns beyond validation: the publish
		// itself failed. The message goes to the wire the way the stop
		// handler's controller failure does, so a browser sees why the start
		// did not land rather than a bare "internal error".
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"request_id": req.RequestID})
}

// startRunBody is the POST /api/runs body: the queue request's own wire
// shape plus an optional attachments array. The bytes are base64 in the
// body but never on the queue request — the handler writes them to the
// store's attachments table and the request carries the ids
// (docs/DATA-API.md). Embedding keeps every request field the queue owns
// flowing through unchanged.
type startRunBody struct {
	queue.Request
	Attachments []startRunAttachment `json:"attachments"`
}

// startRunAttachment is one image a browser start carries: a plain file
// name, a MIME type from ReviewScreenshot's own allowlist, and the image
// bytes base64-encoded.
type startRunAttachment struct {
	Name     string `json:"name"`
	MIMEType string `json:"mime_type"`
	Data     string `json:"data"`
}

// attachmentMaxCountDefault and attachmentMaxBytesDefault back the
// tools.attachments_max_count and tools.attachments_max_bytes settings for
// a Server with no settings resolver (the test path); the registry defaults
// are the same values, and internal/settings/registry_test.go pins them.
const (
	attachmentMaxCountDefault = 8
	attachmentMaxBytesDefault = 5 << 20 // 5 MB per file, ReviewScreenshot's own cap
)

func (s *Server) attachmentMaxCount(ctx context.Context) int {
	if s.Settings != nil {
		if v, err := s.Settings.Int(ctx, settings.KeyToolAttachmentsMaxCount); err == nil {
			return v
		}
	}
	return attachmentMaxCountDefault
}

func (s *Server) attachmentMaxBytes(ctx context.Context) int {
	if s.Settings != nil {
		if v, err := s.Settings.Int(ctx, settings.KeyToolAttachmentsMaxBytes); err == nil {
			return v
		}
	}
	return attachmentMaxBytesDefault
}

// writeAttachments validates each attachment (name, MIME type, base64, the
// per-file byte cap, and the count cap) and stores its bytes, returning the
// ids the request then carries. A nil store — a Server built without one —
// refuses attachments rather than dropping them silently: a run that cannot
// deliver the file its caller sent must not start without it.
func (s *Server) writeAttachments(ctx context.Context, attachments []startRunAttachment) ([]string, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	maxCount := s.attachmentMaxCount(ctx)
	if len(attachments) > maxCount {
		return nil, fmt.Errorf("at most %d attachments are accepted, got %d", maxCount, len(attachments))
	}
	if s.Store == nil {
		return nil, errors.New("attachments cannot be stored: no store is wired")
	}
	maxBytes := s.attachmentMaxBytes(ctx)
	ids := make([]string, 0, len(attachments))
	for _, att := range attachments {
		name, mime, data, err := attachment.Validate(att.Name, att.MIMEType, att.Data, maxBytes)
		if err != nil {
			return nil, err
		}
		id, err := s.Store.WriteAttachment(ctx, name, mime, data)
		if err != nil {
			return nil, fmt.Errorf("store attachment %q: %w", name, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
