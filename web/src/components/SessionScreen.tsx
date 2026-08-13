import { useEffect, useSyncExternalStore, useState } from "react";
import { isUserStarted } from "../api/provenance";
import { useSessionMeta, useTranscriptStore } from "../hooks";
import { SessionChatScreen } from "./SessionChatScreen";
import { SessionWatchScreen } from "./SessionWatchScreen";

interface Props {
  sessionId: string;
  // The app's navigate, passed down for the chat page's follow-up run
  // control (the finished band sends the reader back to the start form).
  onNavigate: (path: string) => void;
}

// The session route's fork: which of the two session pages a run gets
// is decided by SessionState.parent_is_user — true means a person
// started this run and can talk to it (the interactive chat page),
// false means another agent did (the read-only watch page). The decision is
// the single provenance predicate (web/src/api/provenance.ts), the same one
// the "started by" label renders from, so the fork and the label can never
// disagree about the same row.
export function SessionScreen({ sessionId, onNavigate }: Props) {
  const store = useTranscriptStore(sessionId);
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);
  const { meta, settled } = useSessionMeta(sessionId, snapshot.connection);

  // Whether this session's stream has opened since the page loaded, for the
  // dropped-stream banner (DroppedStreamBanner). It lives HERE rather than in
  // the banner or the screens because the fork below can swap one screen for
  // the other when the metadata row lands, taking that screen's local state
  // with it. This component sits above the fork and survives the swap; the
  // flag resets only on a genuine session switch.
  const [everOpen, setEverOpen] = useState(false);
  useEffect(() => {
    if (snapshot.connection === "open") setEverOpen(true);
  }, [snapshot.connection]);
  useEffect(() => {
    setEverOpen(false);
  }, [sessionId]);

  // The app shell: the page stops scrolling and the conversation column
  // scrolls instead. Only this route carries body.app; the session
  // list, settings and operations screens
  // keep scrolling the page as they always have. Within the route, the
  // shell belongs to the chat page and to a live watch run (the pinned
  // footer answers "what is it doing right now"); a finished watch run is
  // an ordinary page again — one scroll, no footer. The mode lives on body
  // as body.app or the page companion body.page (the sticky nav and rail
  // hang off it in styles.css), and follows the run to its end.
  const shell = !settled || meta === null || isUserStarted(meta) || meta.status === "running";
  useEffect(() => {
    if (shell) {
      document.body.classList.add("app");
    } else {
      document.body.classList.add("page");
    }
    return () => {
      document.body.classList.remove("app");
      document.body.classList.remove("page");
    };
  }, [shell]);

  // Before the row arrives the shell renders with the nav and an empty
  // stream — no guessed screen that would then swap to the other one when
  // the row lands. A session whose fetch fails and never yields a row keeps
  // the chat screen, the safe default: a person looking at their own run
  // must never lose the ability to steer it because a fetch failed.
  if (!settled) {
    return (
      <div className="work">
        <main className="stream" />
      </div>
    );
  }
  if (meta === null || isUserStarted(meta)) {
    return (
      <SessionChatScreen
        sessionId={sessionId}
        meta={meta}
        snapshot={snapshot}
        onNavigate={onNavigate}
        everOpen={everOpen}
      />
    );
  }
  return (
    <SessionWatchScreen
      sessionId={sessionId}
      meta={meta}
      snapshot={snapshot}
      onNavigate={onNavigate}
      everOpen={everOpen}
    />
  );
}
