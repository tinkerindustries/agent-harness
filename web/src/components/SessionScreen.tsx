import { useEffect, useSyncExternalStore } from "react";
import { isUserStarted } from "../api/provenance";
import { useSessionMeta, useTranscriptStore } from "../hooks";
import { SessionChatScreen } from "./SessionChatScreen";
import { SessionWatchScreen } from "./SessionWatchScreen";

interface Props {
  sessionId: string;
}

// The session route's fork (design/README.md): which of the two session
// pages a run gets is decided by SessionState.parent_is_user — true means a
// person started this run and can talk to it (the interactive chat page),
// false means another agent did (the read-only watch page). The decision is
// the single provenance predicate (web/src/api/provenance.ts), the same one
// the "started by" label renders from, so the fork and the label can never
// disagree about the same row.
export function SessionScreen({ sessionId }: Props) {
  const store = useTranscriptStore(sessionId);
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot);
  const { meta, settled } = useSessionMeta(sessionId, snapshot.connection);

  // The app shell: the page stops scrolling and the conversation column
  // scrolls instead (design/session.css "app shell"). Only this route
  // carries body.app; the session list, settings and operations screens
  // keep scrolling the page as they always have.
  useEffect(() => {
    document.body.classList.add("app");
    return () => document.body.classList.remove("app");
  }, []);

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
    return <SessionChatScreen sessionId={sessionId} meta={meta} snapshot={snapshot} />;
  }
  return <SessionWatchScreen sessionId={sessionId} meta={meta} snapshot={snapshot} />;
}
