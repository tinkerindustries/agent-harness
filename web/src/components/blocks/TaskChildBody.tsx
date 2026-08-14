import { useEffect, useRef, useSyncExternalStore } from "react";
import { TranscriptStore } from "../../api/transcriptStore";
import { SessionIdContext } from "../../hooks";
import { BlockList } from "../BlockList";

// TaskChildBody owns its own TranscriptStore and SSE connection, exactly
// like the top-level transcript screen (docs/DESIGN.md §4.2: "the same
// endpoint shape" for historical and live). It is a separate module from
// TaskChildTranscript so the latter can load it with a dynamic import —
// see that file for why.
export default function TaskChildBody({ sessionId }: { sessionId: string }) {
  const ref = useRef<TranscriptStore | null>(null);
  if (!ref.current) ref.current = new TranscriptStore(sessionId);

  // The effect owns both halves of the connection, for the reason
  // useTranscriptStore does (hooks.ts).
  useEffect(() => {
    const store = ref.current!;
    store.connect();
    return () => store.disconnect();
  }, []);

  const snapshot = useSyncExternalStore(ref.current.subscribe, ref.current.getSnapshot);
  return (
    <div className="task-child-body">
      {/* Re-provided with the child's own id: a subagent ran in its own
          workspace, so its screenshots resolve against that session and not
          the parent whose transcript this is nested inside. */}
      <SessionIdContext.Provider value={sessionId}>
        <BlockList items={snapshot.items} live={snapshot.live} />
      </SessionIdContext.Provider>
    </div>
  );
}
