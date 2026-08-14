import { useEffect, useRef, useState, useSyncExternalStore, type CSSProperties } from "react";
import { TranscriptStore } from "../api/transcriptStore";
import { liveness, stallNote, type Liveness } from "../api/pulse";
import { RunPulse } from "../components/ui/RunPulse";
import { WatchFooter } from "../components/WatchFooter";
import { makeSeqSource, type SeqSource } from "./syntheticFeed";
import type { SessionState, StoreEvent } from "../api/types";

// The row the footer needs. Only the fields it actually reads matter — the
// model and effort for the pre-first-sub-turn status line, created_at for the
// elapsed clock — and created_at is stamped at module load so the clock is
// counting something.
const HARNESS_META: SessionState = {
  id: "sess-pulse",
  model: "deepseek-v4-pro",
  effort: "high",
  workspace: "/w/sess-pulse",
  permission_mode: "full",
  status: "running",
  created_at: new Date().toISOString(),
  sub_turns: 0,
  version: 1,
  usage: { cost_usd: 0.0421, cache_hit_tokens: 184_000, cache_miss_tokens: 21_000, completion_tokens: 6120, reasoning_tokens: 9840 },
};

// PulseHarness is the run pulse and the stall gate driven by a scripted feed,
// at ?pulse=1. It exists because neither of them can be seen in a unit test
// and neither can be trusted from a screenshot of a resting page: the pulse
// is a shape that only means anything while it moves, and the stall gate's
// whole claim is that the dot changes when the run stops producing. Both
// need a run that misbehaves on cue.
//
// So the script below is a run that misbehaves on cue. It cycles through the
// four states a watcher actually cares about, holding each long enough to
// read, and prints the gate's own numbers beside the dot so the reading can
// be checked against the shape rather than admired.
//
// Nothing here touches the network. The store runs in its connect: false mode
// — the same fold, the same flush, the same meter — and the phases are wall
// clock, because the thing under test is a function of wall clock.

interface Phase {
  name: string;
  ms: number;
  note: string;
  // Characters of each band emitted per tick (TICK_MS), the tool this phase
  // runs (null for a phase where the model is streaming instead), and whether
  // that tool ends in an error.
  reasoning?: number;
  content?: number;
  stdout?: number;
  tool: string | null;
  fail?: boolean;
}

const TICK_MS = 100;

// Roughly what the real thing looks like: a burst of reasoning, prose coming
// out at a steady clip, a build printing far more than either, then nothing
// at all while a tool hangs — which is the state the gate exists for.
// The early phases are there to earn a baseline: the gate withholds a median
// until MIN_SAMPLES tool calls have completed (api/pulse.ts), which is the
// whole reason it cannot cry stall at the first slow thing it sees. So the
// script does four ordinary calls before the one that hangs, and the hang is
// long enough to walk the factor past the cap.
const SCRIPT: Phase[] = [
  { name: "thinking", ms: 4000, note: "reasoning streaming — the dot rests", reasoning: 22, tool: null },
  { name: "Read", ms: 1500, note: "a quick call", tool: "Read" },
  { name: "answering", ms: 3000, note: "prose streaming", content: 30, tool: null },
  { name: "Grep", ms: 2000, note: "another — the median is forming", tool: "Grep" },
  { name: "Read", ms: 1500, note: "the third: a baseline now exists", tool: "Read" },
  { name: "answering", ms: 3000, note: "prose", content: 26, tool: null },
  { name: "Bash", ms: 4000, note: "a build, printing hard", tool: "Bash", stdout: 240 },
  { name: "answering", ms: 2000, note: "prose", content: 24, tool: null },
  { name: "Bash (hung)", ms: 24000, note: "nothing coming out — watch the dot slow", tool: "Bash" },
  { name: "Bash (failed)", ms: 1500, note: "the error mark", tool: "Bash", fail: true },
  { name: "answering", ms: 5000, note: "recovered — the dot rests again", content: 28, tool: null },
];

function ev(kind: StoreEvent["kind"], payload: unknown, seq: SeqSource, atMs: number): StoreEvent {
  return { session_id: "sess-pulse", seq: seq.next(), kind, payload, created_at: new Date(atMs).toISOString() };
}

export function PulseHarness() {
  const storeRef = useRef<TranscriptStore | null>(null);
  if (!storeRef.current) {
    const store = new TranscriptStore("sess-pulse", { connect: false });
    // The meter only records what arrives after the replay seam, so the
    // harness marks it itself — exactly as the other harnesses do, and as
    // web/CLAUDE.md's note on `replayed` says a store driven without a
    // connection must.
    store.markReplayed();
    storeRef.current = store;
  }
  const store = storeRef.current;
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);

  const [phase, setPhase] = useState(SCRIPT[0]);
  const [life, setLife] = useState<Liveness | null>(null);

  useEffect(() => {
    const seq = makeSeqSource();
    let phaseIndex = -1;
    let phaseStartedAt = 0;
    // The tool call currently running, so the next phase can close it and the
    // fold has a pending tool for the gate to age.
    let openTool: string | null = null;
    let subTurn = 0;

    // A tool becomes pending in the fold only through the turn that carried
    // it: turn_started opens the turn, the tool_call joins it, and
    // turn_finished freezes the turn and moves its calls into pendingTools
    // (api/fold.ts). Emitting the call without the turn around it is the
    // shape a real session never produces, and the fold correctly ignores it.
    const startTool = (now: number, name: string) => {
      subTurn++;
      const id = `call-${subTurn}`;
      store.ingest(ev("turn_started", { sub_turn: subTurn }, seq, now));
      store.ingest(ev("tool_call", { index: 0, id, name, arguments: JSON.stringify({ command: `${name.toLowerCase()} scripted` }) }, seq, now));
      // elapsed_ms varies so the sub-turn median is a median of something
      // rather than of one repeated constant.
      store.ingest(
        ev("turn_finished", { sub_turn: subTurn, finish_reason: "tool_calls", elapsed_ms: 3000 + (subTurn % 4) * 900 }, seq, now),
      );
      openTool = id;
    };
    const closeTool = (now: number, isError: boolean) => {
      if (!openTool) return;
      store.ingest(
        ev("tool_result", { tool_call_id: openTool, name: "Bash", content: isError ? "exit 1" : "ok", is_error: isError }, seq, now),
      );
      openTool = null;
    };
    // A streaming phase needs an open turn for the same reason: `kind`
    // reading "turn" is the fold saying there is a live turn, and the live
    // frames below are what fills it.
    const startTurn = (now: number) => {
      subTurn++;
      store.ingest(ev("turn_started", { sub_turn: subTurn }, seq, now));
    };

    const enter = (now: number, index: number) => {
      phaseIndex = index;
      phaseStartedAt = now;
      const next = SCRIPT[index];
      setPhase(next);
      if (next.tool) {
        startTool(now, next.tool);
        // The failing phase is its own call that returns an error at the end
        // of it, not a mark with no call behind it.
        if (next.fail) return;
      } else {
        startTurn(now);
      }
    };

    const id = window.setInterval(() => {
      const now = Date.now();
      if (phaseIndex < 0) {
        enter(now, 0);
        return;
      }
      const current = SCRIPT[phaseIndex];

      if (now - phaseStartedAt >= current.ms) {
        closeTool(now, current.fail === true);
        enter(now, (phaseIndex + 1) % SCRIPT.length);
        return;
      }

      if (current.reasoning) store.ingestLive({ sub_turn: subTurn, channel: "reasoning", text: "x".repeat(current.reasoning) });
      if (current.content) store.ingestLive({ sub_turn: subTurn, channel: "content", text: "y".repeat(current.content) });
      if (current.stdout && openTool) {
        store.ingest(ev("tool_stdout", { tool_call_id: openTool, text: "z".repeat(current.stdout) }, seq, now));
      }
      const snap = store.getSnapshot();
      setLife(liveness(snap.live, snap.durations, now));
    }, TICK_MS);

    return () => {
      window.clearInterval(id);
      store.close();
    };
  }, [store]);

  const note = life ? stallNote(life) : undefined;

  return (
    <div style={{ padding: 24, maxWidth: 840, margin: "0 auto", fontFamily: "var(--font-sans)" }}>
      <h1 style={{ fontSize: "1.1rem", fontWeight: 600, marginBottom: 4 }}>Run pulse harness</h1>
      <p style={{ fontSize: "0.8125rem", color: "var(--muted-foreground)", marginBottom: 20 }}>
        A scripted run through the four states a watcher cares about. The strip is the real component reading a
        real meter off a real store; the figures under it are the stall gate's own, so the dot can be checked
        against the shape rather than admired. The interesting phase is the hung one — watch the dot slow.
      </p>

      <div style={{ border: "1px solid var(--border)", borderRadius: 8, padding: 12, background: "var(--card)" }}>
        <RunPulse meter={snapshot.pulse} className="pulse-strip" />
        <div className="nowline" title={note}>
          <span
            className="dot dot-pulse"
            style={{ color: "var(--status-running)", "--pulse-period": `${Math.round(life?.periodMs ?? 1600)}ms` } as CSSProperties}
            aria-hidden
          />
          <span className="name">{phase.name}</span>
          <span className="arg">{phase.note}</span>
        </div>
      </div>

      <dl style={{ marginTop: 16, fontSize: "0.8125rem", fontFamily: "var(--font-mono)", display: "grid", gridTemplateColumns: "160px 1fr", rowGap: 4 }}>
        <dt>kind</dt>
        <dd data-testid="kind">{life?.kind ?? "—"}</dd>
        <dt>age</dt>
        <dd data-testid="age">{life?.ageMs !== null && life?.ageMs !== undefined ? `${Math.round(life.ageMs)}ms` : "—"}</dd>
        <dt>baseline (median)</dt>
        <dd data-testid="baseline">{life?.baselineMs !== null && life?.baselineMs !== undefined ? `${Math.round(life.baselineMs)}ms` : "— (below the sample floor)"}</dd>
        <dt>stall factor</dt>
        <dd data-testid="factor">{life?.factor !== null && life?.factor !== undefined ? life.factor.toFixed(2) : "—"}</dd>
        <dt>--pulse-period</dt>
        <dd data-testid="period">{Math.round(life?.periodMs ?? 1600)}ms</dd>
        <dt>tooltip</dt>
        <dd data-testid="note">{note ?? "— (nothing to say)"}</dd>
      </dl>

      <p style={{ marginTop: 16, fontSize: "0.75rem", color: "var(--muted-foreground)" }}>
        Bands, bottom to top: tool output (dim), reasoning (violet), prose (blue). Notches along the baseline are
        tool calls starting; a full-height red line is a failure.
      </p>

      {/* The same meter drawn at three times the footer's height. The strip
          in the product is 26px and its whole job is to be read at a glance,
          which is exactly the size at which a drawing bug — a band rounded
          away, a mark landing in the wrong bucket, the scale collapsing after
          a loud phase — is invisible. This copy is where those get caught.
          Nothing about the component changes; only the box it is given. */}
      <style>{`.pulse-strip-big { display: block; width: 100%; height: 78px; }`}</style>
      <h2 style={{ fontSize: "0.875rem", fontWeight: 600, marginTop: 24, marginBottom: 6 }}>The same meter, magnified</h2>
      <div style={{ border: "1px solid var(--border)", borderRadius: 8, padding: 12, background: "var(--card)" }}>
        <RunPulse meter={snapshot.pulse} className="pulse-strip-big" />
      </div>

      {/* The real footer, on the real store, because everything above this is
          the pieces and none of it proves the thing that actually ships. The
          strip has to survive the band's grid, the status line has to still
          fit beside it, and the dot the gate drives has to be the dot the
          footer draws. */}
      <h2 style={{ fontSize: "0.875rem", fontWeight: 600, marginTop: 24, marginBottom: 6 }}>The real WatchFooter</h2>
      <div style={{ border: "1px solid var(--border)", borderRadius: 8, overflow: "hidden" }}>
        <WatchFooter
          meta={HARNESS_META}
          items={snapshot.items}
          live={snapshot.live}
          following
          onToggleFollow={() => {}}
          stop={{ confirming: false, stopping: false, error: null, onCancelStop: () => {}, onConfirmStop: () => {} }}
          getToolCall={snapshot.getToolCall}
          pulse={snapshot.pulse}
          durations={snapshot.durations}
        />
      </div>
    </div>
  );
}
