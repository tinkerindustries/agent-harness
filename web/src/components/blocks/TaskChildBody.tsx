import { useEffect, useRef, useSyncExternalStore } from "react";
import { TranscriptStore } from "../../api/transcriptStore";
import { BlockList } from "../BlockList";

// TaskChildBody owns its own TranscriptStore and SSE connection, exactly
// like the top-level transcript screen (docs/DESIGN.md §4.2: "the same
// endpoint shape" for historical and live). It is a separate module from
// TaskChildTranscript so the latter can load it with a dynamic import —
// see that file for why.
export default function TaskChildBody({ sessionId }: { sessionId: string }) {
  const ref = useRef<TranscriptStore | null>(null);
  if (!ref.current) ref.current = new TranscriptStore(sessionId);

  useEffect(() => {
    const store = ref.current!;
    return () => store.close();
  }, []);

  const snapshot = useSyncExternalStore(ref.current.subscribe, ref.current.getSnapshot);
  return (
    <div className="task-child-body">
      <BlockList blocks={snapshot.blocks} live={snapshot.live} />
    </div>
  );
}
