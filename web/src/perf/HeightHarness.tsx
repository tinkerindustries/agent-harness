import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { TranscriptStore, type TranscriptStoreOptions } from "../api/transcriptStore";
import { TurnTranscript } from "../components/turns/TurnTranscript";
import { PlanPanel } from "../components/PlanPanel";
import { TranscriptToolbar } from "../components/TranscriptToolbar";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import type { TranscriptFilter } from "../api/groups";
import { buildSyntheticHistory, makeSeqSource } from "./syntheticFeed";

// HeightHarness is the page-height measurement harness: mount the
// transcript screen — header, meta row, toolbar, and the turn
// renderer — against a synthetic session the size of the measured one, and
// report the page's scroll height against the 86,674-pixel baseline.
// The real session (sess-f93b37…) lives on the production stack, which a
// measurement must not touch, so the synthetic feed stands in for it
// (web/src/perf holds the harness and the synthetic feed for measuring
// against a session).
//
// Configured by ?blocks=N; the default 391 blocks is the opening block plus
// 142 sub-turns of the standard synthetic cycle (plain/bash/edit/read),
// the measured session's size. A turn is always full — there is no
// Compact/Full toggle to fix, and collapsing happens per tool row — so the
// measurement is of the renderer the screens ship. Nothing here talks to
// the network.

const DEFAULT_BLOCKS = 391; // 1 opening block + 142 synthetic sub-turns

function microtaskScheduler(): TranscriptStoreOptions {
  return {
    scheduleFlush: (cb) => {
      queueMicrotask(() => flushSync(cb));
      return 0;
    },
    cancelFlush: () => {},
  };
}

export function HeightHarness() {
  const params = new URL(window.location.href).searchParams;
  const blocks = Number(params.get("blocks") ?? DEFAULT_BLOCKS);
  const errorDemo = params.get("error") === "1";
  const [store] = useState(() => new TranscriptStore("perf-height", { connect: false, ...microtaskScheduler() }));
  const [seeded, setSeeded] = useState(false);
  const [height, setHeight] = useState<number | null>(null);
  const [subTurns, setSubTurns] = useState(0);
  // StrictMode double-invokes effects in dev; the guard makes seeding
  // idempotent so the fold is not fed the history twice.
  const seededRef = useRef(false);

  useEffect(() => {
    if (seededRef.current) return;
    seededRef.current = true;
    const seq = makeSeqSource();
    const events = buildSyntheticHistory(blocks, "perf-height", seq);
    if (errorDemo) {
      // ?error=1 is a manual-inspection aid: turn the last Bash result into
      // a failure and give the last sub-turn a churn usage, so the error
      // row's open-by-default behaviour and the churn banner can be seen.
      for (let i = events.length - 1; i >= 0; i--) {
        const ev = events[i];
        if (ev.kind === "tool_result") {
          (ev.payload as { is_error?: boolean }).is_error = true;
          break;
        }
      }
      for (let i = events.length - 1; i >= 0; i--) {
        const ev = events[i];
        if (ev.kind === "usage") {
          Object.assign(ev.payload as Record<string, unknown>, {
            prompt_cache_miss_tokens: 600,
            expected_miss_tokens: 100,
            churn_point_index: 1,
          });
          break;
        }
      }
    }
    for (const ev of events) store.ingest(ev);
    // Seeded history is this harness's backlog — close the replay the way
    // the server's `replayed` frame closes a real one.
    store.markReplayed();
    setSubTurns(events.filter((e) => e.kind === "turn_finished").length);
    setSeeded(true);
  }, [store, blocks, errorDemo]);

  return (
    <HeightMount store={store} seeded={seeded} onHeight={setHeight}>
      <p className="perf-height-result" data-testid="perf-height" data-blocks={blocks} data-sub-turns={subTurns}>
        Scroll height: <strong>{height === null ? "…" : height.toLocaleString("en-US")} px</strong>{" "}
        <span className="dim">(baseline 86,674 px, card transcript)</span>
      </p>
    </HeightMount>
  );
}

// HeightMount renders the same chrome as the session screens — header, meta
// row, toolbar, churn banner, turn list, plan column — so the measurement
// is of the session page, not of a bare turn list. filter is fixed at All,
// the product default. The height is measured once, after seeding has
// committed and the transcript has mounted.
function HeightMount({
  store,
  seeded,
  onHeight,
  children,
}: {
  store: TranscriptStore;
  seeded: boolean;
  onHeight: (h: number) => void;
  children: React.ReactNode;
}) {
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);
  const [filter, setFilter] = useState<TranscriptFilter>("all");
  const reported = useRef(false);

  useEffect(() => {
    if (!seeded || snapshot.items.length === 0 || reported.current) return;
    reported.current = true;
    onHeight(document.documentElement.scrollHeight);
  }, [seeded, snapshot.items.length, onHeight]);

  return (
    <div className="screen screen-transcript">
      <header className="screen-header">
        <Button variant="outline" size="sm">
          ← sessions
        </Button>
        <h1>perf-height</h1>
        <Badge variant="outline" className="connection-badge connection-closed">
          closed
        </Badge>
      </header>
      <div className="session-meta">
        <Badge variant="done">DONE</Badge>
        <span className="dim">synthetic feed</span>
        <span className="dim">turn renderer</span>
      </div>
      {children}
      <TranscriptToolbar filter={filter} onFilterChange={setFilter} counts={snapshot.counts} />
      {snapshot.churnPoint && (
        <div className="notice churn-banner">
          <b>
            Cache churn at sub-turn {snapshot.churnPoint.subTurn}: {snapshot.churnPoint.excessTokens.toLocaleString("en-US")} tokens
            re-sent above the expected miss.
          </b>{" "}
          The prefix moved — see docs/CACHE.md.{" "}
          <a href={`#sub-turn-${snapshot.churnPoint.subTurn}`}>Jump to it →</a>
        </div>
      )}
      <div className="transcript-layout">
        <TurnTranscript
          items={snapshot.items}
          live={snapshot.live}
          replayed={snapshot.replayed}
          filter={filter}
          getToolCall={snapshot.getToolCall}
        />
        <PlanPanel todos={snapshot.todos} />
      </div>
    </div>
  );
}
