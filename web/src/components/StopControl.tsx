import { useEffect, useState } from "react";
import { controlToken, errorMessage, stopSession } from "../api/operations";
import { Button } from "./ui/button";

// StopControl is the run's stop control (docs/RUN-CONTROL.md "The
// frontend"): the hard-to-reverse action that ends a run an operator cannot
// get back, behind the same inline-confirm step the operations screen gives
// a delete (no window.confirm — a modal blocks the page and automation
// cannot dismiss it). It sits on the in-flight session card and in the
// transcript header, visible only while the session is running.
//
// Between the 202 and the terminal event the control shows *stopping…*. The
// terminal state arrives over the session's own SSE stream, which the
// screens are already connected to: nothing here polls for it, and nothing
// optimistically marks the session cancelled — the run may still finish on
// its own inside the grace period, and the stream is what says which
// happened. Once the session leaves running the control disappears and the
// status badge renders CANCELLED on the stopped variant (statusBadge.ts).
//
// A null token hides the control: run control not configured means an empty
// bearer would 503, so unavailable must look unavailable, not broken
// (docs/RUN-CONTROL.md "Authentication": a missing credential fails closed).
interface StopControlProps {
  sessionId: string;
  running: boolean;
  className?: string;
}

export function StopControl({ sessionId, running, className }: StopControlProps) {
  const [token, setToken] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The token is fetched once per page load (controlToken caches its
  // promise) and reused for every stop the page makes.
  useEffect(() => {
    let cancelled = false;
    controlToken().then((t) => {
      if (!cancelled) setToken(t);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // A session that stops being running resets the control's local state: the
  // terminal event has arrived, the run is stopped rather than stopping, and
  // an armed confirmation for a run that already ended would be wrong.
  useEffect(() => {
    if (!running) {
      setConfirming(false);
      setStopping(false);
      setError(null);
    }
  }, [running]);

  if (!running || token === null) return null;

  const confirmStop = async () => {
    if (stopping) return;
    setStopping(true);
    setError(null);
    try {
      // The reason is carried verbatim into the cancelled result, where it is
      // the only record of why a run ended. The browser has no free-text
      // field for it, so it says where the stop came from — which is what
      // distinguishes it from a CLI or MCP stop when someone reads the result
      // back later. An empty reason would leave that field blank.
      await stopSession(sessionId, token, "stopped from the browser");
      setConfirming(false);
      // stopping stays true: the run is still ending, and the stream will
      // deliver the terminal state.
    } catch (err) {
      setStopping(false);
      setConfirming(false);
      setError(errorMessage(err));
    }
  };

  return (
    <div className={className}>
      {confirming ? (
        <div className="ops-confirm">
          <span>
            Stop session <code>{sessionId}</code>? The run cannot be got back.
          </span>
          <div className="ops-confirm-buttons">
            <Button variant="destructive" size="sm" onClick={confirmStop} disabled={stopping}>
              Stop
            </Button>
            <Button variant="outline" size="sm" onClick={() => setConfirming(false)} disabled={stopping}>
              Cancel
            </Button>
          </div>
        </div>
      ) : stopping ? (
        <span className="stop-pending dim">stopping…</span>
      ) : (
        <Button variant="destructive" size="sm" onClick={() => setConfirming(true)}>
          Stop
        </Button>
      )}
      {error && <span className="field-error">{error}</span>}
    </div>
  );
}
