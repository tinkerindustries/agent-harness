import type { LiveTurn, PendingTool } from "../../api/fold";
import type { ToolCallPayload } from "../../api/types";
import { useNow, useSkillCatalogue } from "../../hooks";
import { formatElapsed } from "../blocks/ReasoningPanel";
import { skillOpened, toolDetail } from "../blocks/toolArgs";
import { SkillBadge } from "./SkillBadge";
import { StreamText } from "../ui/StreamText";

// LiveTurnSection is the tail of the conversation that has not frozen yet
// (.turn.live): the in-progress sub-turn as a live turn, and any tool
// call awaiting its result as tool-live rows. It is
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
        <div className="mb-6">{toolRows([...pendingTools.values()].map((p) => p.call), pendingTools)}</div>
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
    <div
      className="group relative mb-6 scroll-mt-[var(--nav-height)] pl-10 [&>*+*]:mt-2 max-nav:pl-0"
      id={`sub-turn-${turn.subTurn}`}
    >
      <a
        className="absolute top-px left-0 w-[30px] text-right font-mono text-micro text-muted-foreground tabular-nums no-underline group-hover:text-foreground focus-visible:text-foreground max-nav:static max-nav:mb-0.5 max-nav:block max-nav:w-auto max-nav:text-left max-phone:min-h-11 max-phone:pt-3 max-phone:pb-2"
        href={`#sub-turn-${turn.subTurn}`}
        title={`Sub-turn ${turn.subTurn}`}
      >
        {turn.subTurn}
      </a>
      {reasoning && (
        // "think" stays a literal class only to keep the marker-hiding rule
        // scoped (.think > summary::-webkit-details-marker in styles.css),
        // same as Turn.tsx's own disclosure.
        <details className="think" open>
          <summary className="-ml-1 inline-flex list-none items-center gap-1.5 rounded px-1 py-px text-xs text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:[outline:2px_solid_var(--ring)] focus-visible:outline-offset-1 max-phone:min-h-11 max-phone:px-2 max-phone:py-2.5">
            <span
              className="text-muted-foreground transition-transform [transition-duration:var(--dur-caret)] motion-reduce:transition-none"
              aria-hidden
            >
              ▸
            </span>
            Thinking…{elapsed && ` ${elapsed}`}
          </summary>
          <div className="mt-1.5 border-l border-border pl-[13px] text-sm leading-[1.6] whitespace-pre-wrap text-muted-foreground">
            {reasoning}
            <span className="inline-block h-3.5 w-[7px] align-[-2px] bg-[var(--status-running)] [animation:pulse_var(--cursor-period)_steps(2,start)_infinite] motion-reduce:animate-none" />
          </div>
        </details>
      )}
      {content && (
        <div className="border-l-2 border-[var(--status-running)] pl-[13px] text-md leading-[1.65] max-phone:text-base max-phone:leading-[1.7]">
          <pre className="m-0 font-[inherit] whitespace-pre-wrap wrap-anywhere">
            {/* The reveal rides the live frames, which is the only text here
                that actually arrives a piece at a time: the committed
                content_delta events are written in one batch with the
                turn_finished that freezes this component away
                (internal/session/turn.go), so revealing those would animate a
                whole turn's prose for one frame and then throw it away.
                turn.liveContentChunks is append-only, which is what keeps the
                already-revealed text still — see StreamText. The committed
                fallback has no frames to speak of and renders as it always
                did. */}
            {turn.liveContent ? <StreamText chunks={turn.liveContentChunks} /> : content}
            <span className="inline-block h-3.5 w-[7px] align-[-2px] bg-[var(--status-running)] [animation:pulse_var(--cursor-period)_steps(2,start)_infinite] motion-reduce:animate-none" />
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
// call's own start stamp, so a running row can count its own age
// ("running · 42s") the way the footer's nowline does. A
// streaming turn's announced calls have not started running yet and carry
// neither. Two or more rows read as a set, wrapped in the same
// bordered/labelled group Turn.tsx's toolset uses.
function toolRows(calls: ToolCallPayload[], pendingTools: Map<string, PendingTool> | null): React.ReactNode {
  const rows = calls.map((call) => (
    <LiveToolRow key={call.id} call={call} pending={pendingTools?.get(call.id)} />
  ));
  if (rows.length >= 2) {
    return (
      <div className="border-l border-border pl-[11px]">
        <div className="mb-[5px] text-micro text-muted-foreground">{rows.length} calls in parallel</div>
        {rows}
      </div>
    );
  }
  return rows;
}

// LiveToolRow is one running tool call (.tool.tool-live): open by
// default, pulsing dot, target, "running" plus
// the wall time so far when the fold knows when the call started, and the
// streamed stdout as plain preformatted text when there is any. "tool"
// stays a literal class only to keep the marker-hiding rule scoped, same as
// Turn.tsx's ToolRow — everything else is a direct Tailwind utility, always
// in the "running" colour since a live row has no other state.
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
  const catalogue = useSkillCatalogue();
  const skill = skillOpened(call, catalogue);
  const age = pending?.startedAt ? formatElapsed(now - Date.parse(pending.startedAt)) : "";
  return (
    <details className="tool rounded-[calc(var(--radius)-2px)] border border-[var(--status-running)] bg-card [&+&]:mt-1" open>
      <summary className="flex list-none items-center gap-2 rounded-[calc(var(--radius)-3px)] px-2.5 py-[7px] text-sm cursor-pointer hover:bg-accent focus-visible:[outline:2px_solid_var(--ring)] focus-visible:outline-offset-[-2px] max-phone:min-h-11 max-phone:px-3 max-phone:py-2.5">
        <span aria-hidden className="flex w-[13px] flex-none justify-center text-center text-xs text-[var(--status-running)]">
          <span className="h-1.5 w-1.5 flex-none rounded-full bg-current dot-pulse" />
        </span>
        <span className="flex-none font-mono text-xs font-semibold">{call.name}</span>
        {target && <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">{target}</span>}
        {skill && <SkillBadge name={skill} />}
        <span className="flex-none text-micro tabular-nums text-[var(--status-running)]">
          running{age ? ` · ${age}` : ""}
        </span>
      </summary>
      {pending?.stdout && (
        <pre className="m-0 border-t border-border px-2.5 py-2 font-mono text-xs leading-[1.55] whitespace-pre-wrap wrap-anywhere text-muted-foreground max-phone:text-sm">
          {pending.stdout}
          <span className="inline-block h-3.5 w-[7px] align-[-2px] bg-[var(--status-running)] [animation:pulse_var(--cursor-period)_steps(2,start)_infinite] motion-reduce:animate-none" />
        </pre>
      )}
    </details>
  );
}
