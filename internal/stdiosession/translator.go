package stdiosession

import (
	"encoding/json"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// A Translator turns one run's committed event log, and the live text deltas
// published alongside it, into one surface's notifications — and holds the
// document those notifications assemble into, so a get can answer mid-run.
//
// The two sources say different things and both are needed. The event log is
// authoritative and ordered but arrives one batch per sub-turn, so a client
// fed only from it would see a sub-turn's whole answer appear at once. The
// live deltas (internal/hub) carry the same text as it is produced,
// coalesced on an interval and always flushed at the end of a sub-turn. So
// text streams from the deltas and structure — tool calls, results, thought
// signatures, usage, the run's end — comes from the log.
//
// Everything but Resource and Result is called on the one goroutine draining
// the pump, and SetMessageID on the append handler's. Resource and Result are
// called from a client's own goroutine while the run is still going, so an
// implementation guards its assembled document with its own lock.
type Translator interface {
	// Created emits the first frame of the stream.
	Created()

	// Live folds in one coalesced live delta: the answer text and the
	// model's reasoning as they are produced.
	Live(d hub.LiveDelta)

	// Event folds in one committed log event.
	Event(e store.Event)

	// CloseText closes whichever streamed items are still open. The run's
	// last sub-turn has no successor to close them.
	CloseText()

	// UserInput records the input the run opened with, or one an append
	// added. source is `input`, `append` or `reminder`.
	UserInput(text, source, messageID string)

	// SetFirstMessageID records the client's own id for the create's input;
	// SetMessageID does the same for an append, keyed by the log seq the
	// append landed at. Both are carried back on the frames that describe
	// the message, so a parent can match what it sent to what it reads.
	SetFirstMessageID(id string)
	SetMessageID(seq int64, id string)

	// Resource renders the run as this surface's own resource — a Response,
	// or an Interaction — from the run's neutral facts and the translator's
	// assembled output.
	Resource(v RunView) any

	// Result wraps Resource as the body a create, cancel or get answers
	// with.
	Result(v RunView) any

	// Completed and Failed emit the two frames a run can end on. They are
	// the translator's rather than the server's because the sequence
	// counter is here, and because the two surfaces disagree about the
	// payload as well as the name: one carries the whole resource where the
	// other carries the id and the error alone.
	Completed(v RunView)
	Failed(v RunView)
}

// RunView is everything the server knows about a run when it asks for a
// resource. What each surface calls these, and which of them it puts in its
// own harness block, is the translator's business.
type RunView struct {
	ID        string
	Model     string
	Status    string
	SessionID string
	Created   time.Time
	Updated   time.Time

	// Reason, Text, Result and SubTurns are what the loop finished with
	// (internal/session, RunResult). They are empty until it has.
	Reason   string
	Text     string
	Result   json.RawMessage
	SubTurns int
	Err      *RunError

	// UnappliedMessageIDs are the client's ids for the steers withdrawn when
	// the run ended, in commit order. Empty until it has ended.
	UnappliedMessageIDs []string

	// WithItems asks for the whole output array. A streaming create's first
	// answer leaves it false; get, cancel and a stream:false create set it.
	WithItems bool
}

// RunError is a failed run's cause, before a surface spells it. Both spell
// it {code, message}.
type RunError struct {
	Code    string
	Message string
}
