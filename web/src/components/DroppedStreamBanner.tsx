import type { ConnectionState } from "../api/transcriptStore";

// DroppedStreamBanner is the "this page stopped receiving updates" banner
// (design/session-states.html "Both pages · the stream stopped arriving"),
// mounted by both session screens above the work band. The SSE connection
// dropping is not the run failing, and the page must not imply that it is:
// the transcript already shown stays exactly as it is, and the banner says
// only what is actually true — that this page is no longer being told what
// happens next. The run is unaffected, and the browser's EventSource
// reconnects on its own (its default retry is ~3s), so the banner needs no
// button of its own: the stream either comes back or the run's terminal
// event closes it for good, and the connection badge in the nav tracks both.
//
// The store's connection state is "connecting" both before the stream first
// opens and after a drop, so the banner distinguishes the two by whether the
// stream has ever opened. That flag cannot live here: useSessionMeta
// refetches on every connection change (its refreshOn is the connection
// state), and each refetch flips the route's settled flag, remounting the
// screen — and with it, any local state. SessionScreen owns the flag
// instead, above the fork, where it survives the remounts; a page that
// never connected shows nothing while the replay loads, and a page that
// connected and then lost the stream shows the banner. "closed" is the
// terminal state — the run is over, the stream ended on purpose — and shows
// nothing.
export function DroppedStreamBanner({
  connection,
  everOpen,
}: {
  connection: ConnectionState;
  // Whether this session's stream has opened at least once since the page
  // loaded (SessionScreen's per-session flag).
  everOpen: boolean;
}) {
  if (!everOpen || connection !== "connecting") return null;
  return (
    <div className="banner">
      <span className="dot" style={{ color: "var(--status-gaveup)" }} aria-hidden />
      <span>
        <b>Not receiving updates.</b> The run is unaffected — this page lost its connection. Reconnecting…
      </span>
    </div>
  );
}
