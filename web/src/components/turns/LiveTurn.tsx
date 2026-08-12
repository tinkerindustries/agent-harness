import type { LiveTurn, PendingTool } from "../../api/fold";
import type { ToolCallPayload } from "../../api/types";
import { useNow } from "../../hooks";
import { formatElapsed } from "../blocks/ReasoningPanel";
import { toolDetail } from "../blocks/toolArgs";

// LiveTurnSection is the tail of the conversation that has not frozen yet
// (design/session-chat.html's .turn.live): the in-progress sub-turn as a
// live turn, and any tool call awaiting its result as tool-live rows. It is
// deliberately NOT memoised — it re-renders on every flush, which is the
// point (docs/DESIGN.md §5.3: streaming content renders as plain
// preformatted text — no markdown parse, no highlighting, no diff — and the
// cursor marks the growing end).
export function LiveTurnSection({ turn, pendingTools }: { turn: LiveTurn | null; pendingTools: Map<string, PendingTool> }) {
  return (
    <>
      {turn && <LiveTurn turn={turn} />}
      {pendingTools.size > 0 && (
        // A frozen turn awaiting its results: its tool rows live here, after
        // the turn they belong to, until each result lands and the group
        // absorbs it. The wrapper carries the turn's bottom margin so the
        // next turn starts on the same rhythm.
        <div className="pending-live">{toolRows([...pendingTools.values()].map((p) => p.call), pendingTools)}</div>
      )}
    </>
  );
}

// LiveTurn is the streaming sub-turn: same shape as a frozen turn — gutter,
// think open, prose, tool rows — but the think says "Thinking…" with the
// wall time so far, the prose is a plain pre, and a cursor marks where the
// text is still arriving. The whole thing is discarded the moment
// turn_finished freezes the group.
function LiveTurn({ turn }: { turn: LiveTurn }) {
  const elapsed = turn.startedAt ? formatElapsed(Date.now() - Date.parse(turn.startedAt)) : "";
  // The live pair is what actually streams: turn_started commits before the
  // request, then `live` frames carry the text, and the committed
  // reasoning_delta/content_delta events arrive in the same batch as the
  // turn_finished that freezes this component away. The committed pair is
  // the fallback for the one frame where a batch is half-ingested, and for
  // any path that replays a log without live frames.
  const reasoning = turn.liveReasoning || turn.reasoning;
  const content = turn.liveContent || turn.content;
  return (
    <div className="turn live" id={`sub-turn-${turn.subTurn}`}>
      <a className="gutter" href={`#sub-turn-${turn.subTurn}`} title={`Sub-turn ${turn.subTurn}`}>
        {turn.subTurn}
      </a>
      {reasoning && (
        <details className="think" open>
          <summary>
            <span className="caret" aria-hidden>
              ▸
            </span>
            Thinking…{elapsed && ` ${elapsed}`}
          </summary>
          <div className="thought">
            {reasoning}
            <span className="cursor" />
          </div>
        </details>
      )}
      {content && (
        <div className="say">
          <pre>
            {content}
            <span className="cursor" />
          </pre>
        </div>
      )}
      {turn.toolCalls.length > 0 && toolRows(turn.toolCalls, null)}
    </div>
  );
}

// toolRows renders tool rows with the live vocabulary — a pulsing dot for
// the glyph and a "running" timing, the design's sub-turn 43. pendingTools,
// when given, supplies the streamed stdout for the calls it knows — and the
// call's own start stamp, so a running row can count its own age ("running
// · 42s", design/session-watch.html) the way the footer's nowline does. A
// streaming turn's announced calls have not started running yet and carry
// neither. Two or more rows read as a set, wrapped in a .toolset.
function toolRows(calls: ToolCallPayload[], pendingTools: Map<string, PendingTool> | null): React.ReactNode {
  const rows = calls.map((call) => (
    <LiveToolRow key={call.id} call={call} pending={pendingTools?.get(call.id)} />
  ));
  if (rows.length >= 2) {
    return (
      <div className="toolset">
        <div className="setlabel">{rows.length} calls in parallel</div>
        {rows}
      </div>
    );
  }
  return rows;
}

// LiveToolRow is one running tool call (design/session-chat.html's
// .tool.tool-live): open by default, pulsing dot, target, "running" plus
// the wall time so far when the fold knows when the call started, and the
// streamed stdout as plain preformatted text when there is any.
//
// The age ticks on its own clock rather than riding the flush loop, which is
// the difference between a counter and a timestamp: the section around it
// re-renders when an event arrives, and a tool that produces no output —
// exactly the long `sleep`, the slow test run, the wedged clone an operator
// is watching this row to judge — produces none for as long as it runs. Read
// off Date.now() at render, "running · 8s" froze at 8s until the result
// landed, which reads as a stalled tool rather than a working one. The watch
// footer's nowline already counts the same call from the same stamp against
// a ticking clock (WatchFooter's useNow), and the two disagreeing on screen
// is worse than either being wrong alone. A handful of rows ticking once a
// second is the case useNow's own comment sanctions — this is not the
// per-token frame budget web/CLAUDE.md is about.
function LiveToolRow({ call, pending }: { call: ToolCallPayload; pending?: PendingTool }) {
  const now = useNow(1000);
  const target = toolDetail(call);
  const age = pending?.startedAt ? formatElapsed(now - Date.parse(pending.startedAt)) : "";
  return (
    <details className="tool tool-live" open>
      <summary>
        <span className="glyph" aria-hidden>
          <span className="dot dot-pulse" />
        </span>
        <span className="name">{call.name}</span>
        {target && <span className="target">{target}</span>}
        <span className="timing">running{age ? ` · ${age}` : ""}</span>
      </summary>
      {pending?.stdout && (
        <pre className="tool-out">
          {pending.stdout}
          <span className="cursor" />
        </pre>
      )}
    </details>
  );
}
