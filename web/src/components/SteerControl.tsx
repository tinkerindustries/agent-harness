import { useEffect, useState } from "react";
import { controlToken, errorMessage, steerSession } from "../api/operations";
import { Button } from "./ui/button";
import { Input } from "./ui/input";

// SteerControl is the run's steer input (docs/RUN-CONTROL.md "The
// frontend"): a text box on the transcript screen, visible only while the
// session is running, that appends an operator instruction the loop picks up
// at its next sub-turn boundary. The run does not stop to read it — a long
// tool call in flight can delay delivery by minutes — and the transcript's
// steer block shows the text as *pending* until the matching steer_applied
// arrives, which is exactly how an operator tells a wedged run from a busy
// one.
//
// The write is an acceptance, not a delivery: the 202 only means the text
// landed in the log. Nothing here polls or guesses at when the model saw it;
// the SSE stream the screens are already connected to delivers the
// steer_applied event that flips the block to delivered.
//
// A null token hides the control, exactly as it hides StopControl: run
// control not configured means an empty bearer would 503, so unavailable
// must look unavailable, not broken (docs/RUN-CONTROL.md "Authentication": a
// missing credential fails closed).
interface SteerControlProps {
  sessionId: string;
  running: boolean;
  className?: string;
}

export function SteerControl({ sessionId, running, className }: SteerControlProps) {
  const [token, setToken] = useState<string | null>(null);
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The token is fetched once per page load (controlToken caches its
  // promise) and reused for every steer the page makes.
  useEffect(() => {
    let cancelled = false;
    controlToken().then((t) => {
      if (!cancelled) setToken(t);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // A session that stops being running clears the input: a steer for a
  // finished run would sit in the log forever, unapplied and unexplained,
  // and the endpoint would refuse it anyway.
  useEffect(() => {
    if (!running) {
      setText("");
      setSending(false);
      setError(null);
    }
  }, [running]);

  if (!running || token === null) return null;

  const send = async () => {
    if (sending || text.trim() === "") return;
    setSending(true);
    setError(null);
    try {
      await steerSession(sessionId, token, text);
      // The 202 is the acceptance, not the delivery: the text appears in the
      // transcript as a pending steer block via the SSE stream the moment
      // the event lands, and flips to delivered when the loop applies it.
      setText("");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSending(false);
    }
  };

  return (
    <div className={`steer-control ${className ?? ""}`}>
      <Input
        className="steer-input"
        value={text}
        placeholder="Steer the run… (reaches the model at the next sub-turn boundary)"
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey) {
            e.preventDefault();
            void send();
          }
        }}
        disabled={sending}
        aria-label="Steer the running session"
      />
      <Button variant="outline" size="sm" onClick={send} disabled={sending || text.trim() === ""}>
        {sending ? "Sending…" : "Send"}
      </Button>
      {error && <span className="field-error">{error}</span>}
    </div>
  );
}
