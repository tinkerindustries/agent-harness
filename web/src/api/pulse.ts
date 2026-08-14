import type { LiveDelta, LiveView } from "./fold";
import type { StoreEvent, ToolCallPayload, ToolDeniedPayload, ToolResultPayload, ToolStdoutPayload, TurnFinishedPayload } from "./types";

// The liveness layer: two readings of "is this run actually moving", both
// taken off the same event stream the transcript folds and neither of them a
// field the server sends.
//
//   - PulseMeter is the run pulse's data — a rolling window of how much text
//     arrived when, bucketed by wall clock, drawn as a canvas strip
//     (components/ui/RunPulse.tsx). It answers the question the footer's
//     "Thinking · 1m 04s" cannot: is the run producing anything right now.
//   - DurationStats is the stall gate — the session's own median tool call
//     and sub-turn, which is what the current activity's age gets measured
//     against to decide how fast the live dot should breathe (pulsePeriodMs).
//
// Both live outside React state, for the reason docs/DESIGN.md §5.2 gives:
// they are fed from the delta path, at token rate, and a React update per
// delta is the thing this app is built not to do. The meter is read by a
// canvas on its own animation frame and never enters a snapshot as a value;
// it rides one as a stable reference, the way getToolCall does.

// ---------- the run pulse ----------

// The window is what a spectator can usefully see at once: long enough that a
// two-minute Bash call reads as a flat stretch rather than as the whole
// picture, short enough that each bucket is still a visible bar at footer
// width. 500ms buckets put ~10 live frames in each while prose streams (the
// server coalesces live output to one frame per channel per 100ms —
// internal/session/turn.go liveFlushInterval), so a bucket is a real sample
// and not a single frame's noise.
export const BUCKET_MS = 500;
export const WINDOW_MS = 90_000;
export const BUCKETS = WINDOW_MS / BUCKET_MS;

export type PulseBand = "reasoning" | "content" | "stdout";

// PulseFrame is one read of the window, ordered oldest bucket first so a
// drawing pass can walk it left to right. Every array is BUCKETS long.
//
// The arrays are the meter's own storage, reused between reads: this is read
// once per animation frame for as long as a run is live, and handing back
// five fresh arrays each time would make the one component in this app that
// deliberately avoids allocation the one that allocates most. Copy them if
// you need to keep them past the next read — the tests do.
export interface PulseFrame {
  reasoning: Uint32Array;
  content: Uint32Array;
  stdout: Uint32Array;
  // How many tool calls started in each bucket, and how many of them (or of
  // the results that landed there) were errors. Drawn as marks along the
  // baseline rather than as another band: a tool call is an event, not a
  // rate, and stacking it on top of a character count would say it had a
  // size.
  tools: Uint8Array;
  errors: Uint8Array;
}

// PulseMeter accumulates the window. Every write goes through advance(), so
// the ring's head is always the bucket containing `nowMs` and every bucket
// between the old head and the new one has been cleared — a run that goes
// quiet for a minute scrolls its own silence through the window rather than
// leaving a minute-old bar sitting at the right-hand edge.
export class PulseMeter {
  private reasoning = new Uint32Array(BUCKETS);
  private content = new Uint32Array(BUCKETS);
  private stdout = new Uint32Array(BUCKETS);
  private tools = new Uint8Array(BUCKETS);
  private errors = new Uint8Array(BUCKETS);

  // head is the ring index of the newest bucket; headAt is that bucket's
  // start instant. Both are set by the first write rather than at
  // construction: a meter built when the page loads and first written to
  // thirty seconds later must not open with thirty seconds of scrolled-past
  // silence it was never actually watching.
  private head = 0;
  private headAt = 0;
  private started = false;

  // The ordered output of a read, reused (see PulseFrame).
  private out: PulseFrame = {
    reasoning: new Uint32Array(BUCKETS),
    content: new Uint32Array(BUCKETS),
    stdout: new Uint32Array(BUCKETS),
    tools: new Uint8Array(BUCKETS),
    errors: new Uint8Array(BUCKETS),
  };

  // rev counts every change to the window: a write, and each bucket the head
  // moves on by. It is what lets the strip's animation frame decide it has
  // nothing to do — see `revision`.
  private rev = 0;

  // hasData is whether anything has ever been recorded. The strip renders a
  // resting line rather than an empty box until it is true, which is the
  // honest state for the first moments of a page opened mid-run: this meter
  // only ever holds what arrived while somebody was watching.
  get hasData(): boolean {
    return this.started;
  }

  // revision is a monotonic counter a drawing pass can compare against its
  // last one to skip a redraw that would produce an identical picture.
  //
  // Without it the strip repaints sixty times a second for as long as a run
  // is live, and the picture only actually changes when a live frame lands
  // (about ten times a second, since the server coalesces to one frame per
  // channel per 100ms) or when the window scrolls on a bucket (twice a
  // second). A run sitting on a long Bash call would repaint sixty times a
  // second to draw the same bars — which is the shape of waste every rule in
  // web/CLAUDE.md's frame-budget section exists to prevent, and it would be a
  // poor joke to reintroduce it in the component that reports liveness.
  get revision(): number {
    return this.rev;
  }

  // text records model output. The live channel is the only one that arrives
  // a piece at a time — the committed reasoning_delta/content_delta pair is
  // written in one batch with the turn_finished that freezes the block
  // (web/CLAUDE.md, "A `live` SSE frame is not an event") — so feeding both
  // would draw every sub-turn twice, once smeared across the time it took and
  // once as a spike at the end of it.
  text(nowMs: number, band: PulseBand, chars: number): void {
    if (chars <= 0) return;
    this.advance(nowMs);
    const bucket = band === "reasoning" ? this.reasoning : band === "content" ? this.content : this.stdout;
    bucket[this.head] += chars;
    this.rev++;
  }

  toolStarted(nowMs: number): void {
    this.advance(nowMs);
    if (this.tools[this.head] < 255) this.tools[this.head]++;
    this.rev++;
  }

  errored(nowMs: number): void {
    this.advance(nowMs);
    if (this.errors[this.head] < 255) this.errors[this.head]++;
    this.rev++;
  }

  // feedLive and feedEvent are the two wiring points TranscriptStore calls,
  // kept here rather than in the store so the mapping from event kind to band
  // is one thing in one place and can be tested without a store.
  feedLive(nowMs: number, d: LiveDelta): void {
    this.text(nowMs, d.channel === "reasoning" ? "reasoning" : "content", d.text.length);
  }

  feedEvent(nowMs: number, ev: StoreEvent): void {
    switch (ev.kind) {
      case "tool_stdout":
        this.text(nowMs, "stdout", (ev.payload as ToolStdoutPayload).text.length);
        break;
      case "tool_call":
        this.toolStarted(nowMs);
        break;
      case "tool_result":
        if ((ev.payload as ToolResultPayload).is_error) this.errored(nowMs);
        break;
      case "tool_denied":
      case "error":
        this.errored(nowMs);
        break;
      default:
        // Everything else is either already counted through the live channel
        // or is not activity: usage, turn boundaries, steer bookkeeping.
        break;
    }
  }

  // read returns the window ordered oldest first. It advances first, so a
  // caller that only ever reads (a live run that has gone quiet) still sees
  // the silence scroll.
  read(nowMs: number): PulseFrame {
    this.advance(nowMs);
    const { out } = this;
    // The oldest bucket is the one after the head, wrapping.
    for (let i = 0; i < BUCKETS; i++) {
      const src = (this.head + 1 + i) % BUCKETS;
      out.reasoning[i] = this.reasoning[src];
      out.content[i] = this.content[src];
      out.stdout[i] = this.stdout[src];
      out.tools[i] = this.tools[src];
      out.errors[i] = this.errors[src];
    }
    return out;
  }

  private advance(nowMs: number): void {
    if (!this.started) {
      this.started = true;
      this.head = 0;
      // Anchor the head to the bucket grid so the bars do not drift relative
      // to each other as the window scrolls.
      this.headAt = Math.floor(nowMs / BUCKET_MS) * BUCKET_MS;
      this.rev++;
      return;
    }
    let steps = Math.floor((nowMs - this.headAt) / BUCKET_MS);
    if (steps <= 0) return;
    // A gap longer than the window clears everything; without the cap a tab
    // left in the background for an hour would spin 7,200 iterations on the
    // frame it came back.
    if (steps > BUCKETS) steps = BUCKETS;
    for (let i = 0; i < steps; i++) {
      this.head = (this.head + 1) % BUCKETS;
      this.reasoning[this.head] = 0;
      this.content[this.head] = 0;
      this.stdout[this.head] = 0;
      this.tools[this.head] = 0;
      this.errors[this.head] = 0;
    }
    this.headAt = Math.floor(nowMs / BUCKET_MS) * BUCKET_MS;
    this.rev++;
  }
}

// ---------- the stall gate ----------

// --pulse-period's resting value, matching the token in styles.css. The
// constant is duplicated rather than read back out of the cascade because
// this is the value the interpolation starts from and a getComputedStyle per
// frame to recover it would cost a layout read for a number that never
// changes.
export const PULSE_NORMAL_MS = 1600;
// The slowest the dot ever breathes. Deliberately not a stop: a run that has
// stalled is still a run, and a dot that freezes reads as a dead page rather
// than as a slow one.
export const PULSE_STALLED_MS = 3600;

// How many times its own median an activity has to run before the dot is at
// its slowest. Four is roughly where a watcher starts to wonder rather than
// merely notice.
export const STALL_FACTOR_MAX = 4;

// Nothing is called slow before this, however fast the session's median is.
// A session whose tool calls all return in 40ms would otherwise put a
// perfectly ordinary one-second call at 25x and throb as though it had hung.
export const MIN_BASELINE_MS = 2000;

// The number of completed samples before a median is trusted. Two samples
// have no middle worth the name, and the first tool call of a run would
// otherwise become the standard the second is judged against.
export const MIN_SAMPLES = 3;

// How many samples of each kind are kept. A long run's early tool calls say
// little about what is normal for it now — the plan has moved on and so has
// the size of the work — so the median tracks a window rather than the run.
const MAX_SAMPLES = 50;

// stallFactor is how many times its own baseline the current activity has
// been running: 1 is exactly normal, below 1 is quicker than normal, and null
// means there is nothing to compare (no activity, or not enough completed
// samples to have a median yet).
export function stallFactor(ageMs: number | null, baselineMs: number | null): number | null {
  if (ageMs === null || baselineMs === null) return null;
  return ageMs / Math.max(baselineMs, MIN_BASELINE_MS);
}

// pulsePeriodMs maps that factor onto the --pulse-period the live dot
// breathes at, linearly between the resting value and the slowest one. An
// unknown factor rests: the gate exists to say "this is taking longer than
// this run's normal", and with no normal to compare against it has nothing to
// say and must not invent an alarm.
export function pulsePeriodMs(factor: number | null): number {
  if (factor === null || factor <= 1) return PULSE_NORMAL_MS;
  const t = Math.min((factor - 1) / (STALL_FACTOR_MAX - 1), 1);
  return PULSE_NORMAL_MS + t * (PULSE_STALLED_MS - PULSE_NORMAL_MS);
}

// median over an unsorted sample list, without mutating it. Below
// MIN_SAMPLES it returns null rather than a number nobody should act on.
export function median(samples: readonly number[]): number | null {
  if (samples.length < MIN_SAMPLES) return null;
  const sorted = [...samples].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  return sorted.length % 2 === 1 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

// DurationStats keeps the two baselines the stall gate compares against: how
// long this session's tool calls take, and how long its sub-turns take.
//
// Unlike PulseMeter this is fed during the history replay as well as live,
// and off the events' own created_at rather than the wall clock. The two
// choices go together and both follow from what each structure is for: the
// meter draws what happened while somebody was watching, so replaying a
// finished session's backlog into it would land the whole run in one bucket;
// the baselines are a statement about the session, so a page opened forty
// sub-turns into a run should arrive already knowing what normal looks like
// there rather than spending the next few minutes learning it.
export class DurationStats {
  // Tool call id to its start instant, for calls whose result has not landed.
  private pending = new Map<string, number>();
  private toolMs: number[] = [];
  private turnMs: number[] = [];

  feedEvent(ev: StoreEvent): void {
    switch (ev.kind) {
      case "tool_call": {
        const p = ev.payload as ToolCallPayload;
        const at = Date.parse(ev.created_at);
        if (p.id && Number.isFinite(at)) this.pending.set(p.id, at);
        break;
      }
      case "tool_result":
      case "tool_denied": {
        const p = ev.payload as ToolResultPayload | ToolDeniedPayload;
        const start = this.pending.get(p.tool_call_id);
        if (start === undefined) break;
        this.pending.delete(p.tool_call_id);
        const end = Date.parse(ev.created_at);
        // A negative or non-finite span is a clock artefact, not a
        // measurement; it would drag the median toward zero and make
        // everything after it look stalled.
        if (Number.isFinite(end) && end >= start) push(this.toolMs, end - start);
        break;
      }
      case "turn_finished": {
        const p = ev.payload as TurnFinishedPayload;
        // Absent on sessions committed before the field existed, which is
        // simply a session with no sub-turn baseline.
        if (typeof p.elapsed_ms === "number" && p.elapsed_ms >= 0) push(this.turnMs, p.elapsed_ms);
        break;
      }
      default:
        break;
    }
  }

  // The baseline for a running tool call, and for a streaming sub-turn.
  // Null until MIN_SAMPLES of that kind have completed.
  toolBaselineMs(): number | null {
    return median(this.toolMs);
  }

  turnBaselineMs(): number | null {
    return median(this.turnMs);
  }
}

function push(samples: number[], value: number): void {
  samples.push(value);
  if (samples.length > MAX_SAMPLES) samples.shift();
}

// ---------- what the two of them add up to ----------

// Liveness is the reading the live dot breathes at, and the sentence behind
// it. One helper rather than the same arithmetic in the footer and in the
// nav, because the two carry the same dot for the same run and a page where
// they disagreed about whether it had stalled would be worse than a page with
// no gate at all.
export interface Liveness {
  // How long the current activity has been going, or null between sub-turns
  // where there is nothing to age.
  ageMs: number | null;
  // What this session's comparable activity usually takes: the median tool
  // call while one is running, the median sub-turn while the model streams.
  // Null until the session has enough completed samples of that kind.
  baselineMs: number | null;
  factor: number | null;
  // The --pulse-period to set on the dot.
  periodMs: number;
  // The kind of activity being aged, for the sentence a reader gets on hover.
  kind: "tool" | "turn" | "idle";
}

export function liveness(live: LiveView, durations: DurationStats, nowMs: number): Liveness {
  let ageMs: number | null = null;
  let baselineMs: number | null = null;
  let kind: Liveness["kind"] = "idle";

  if (live.pendingTools.size > 0) {
    // The last call to start is the one the footer names, so it is the one
    // aged. A sub-turn that fired several calls at once has them all running
    // from the same instant anyway.
    const pending = [...live.pendingTools.values()];
    const started = Date.parse(pending[pending.length - 1].startedAt);
    if (Number.isFinite(started)) ageMs = nowMs - started;
    baselineMs = durations.toolBaselineMs();
    kind = "tool";
  } else if (live.turn) {
    const started = Date.parse(live.turn.startedAt);
    if (Number.isFinite(started)) ageMs = nowMs - started;
    baselineMs = durations.turnBaselineMs();
    kind = "turn";
  }

  const factor = stallFactor(ageMs, baselineMs);
  return { ageMs, baselineMs, factor, periodMs: pulsePeriodMs(factor), kind };
}

// stallNote is the sentence a reader gets when they hover the dot, and the
// only place the gate explains itself. It is deliberately absent below the
// threshold: a dot that is breathing normally has nothing to say, and a
// tooltip that reads "this is fine" on every hover teaches people to stop
// hovering.
export function stallNote(life: Liveness): string | undefined {
  if (life.factor === null || life.factor < 1.5 || life.kind === "idle") return undefined;
  const what = life.kind === "tool" ? "tool call" : "sub-turn";
  return `${life.factor.toFixed(1)}× this run's median ${what}`;
}
