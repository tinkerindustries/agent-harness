import { useState } from "react";
import type { Block } from "../api/fold";
import { Button } from "./ui/button";

export type RunFinishedBlock = Extract<Block, { type: "run_finished" }>;

// ResultPanel is the watch page's rendering of the run_finished block
// ("Watching · the result the parent gets back"): for a run another
// agent launched, the result payload is the point — it is what gets
// returned to the caller — so it renders at the end of the stream,
// where the run actually ended, rather than in a panel off to one side. It
// carries a copy control because the next thing a person does with a payload
// is paste it somewhere.
//
// Two shapes, matching the two terminal payloads a run can hand back: the
// done variant names the agent it is returned to and shows the model's
// summary and the result JSON; the gave-up variant (Complete("gave_up"))
// states the outcome in its own colour. Whatever the shape, the panel says
// only what the log says — the summary, the result, nothing invented.
// A payload of more than this many lines opens collapsed: the summary above
// it is the answer a reader wants first, and a 300-line JSON dump between
// the run's last sub-turn and the end of the page buries it. Anything
// shorter is quicker to read than to click.
const COLLAPSE_OVER_LINES = 16;

export function ResultPanel({ block, parentAgent }: { block: RunFinishedBlock; parentAgent?: string }) {
  const gaveUp = block.status === "gave_up";
  const summary = block.summary || block.text;
  const json = block.result !== undefined ? JSON.stringify(block.result, null, 2) : null;
  const lines = json === null ? 0 : json.split("\n").length;

  return (
    <section className={`result${gaveUp ? " result-gaveup" : ""}`}>
      <div className="result-head">
        <h4>{gaveUp ? "Result · gave up" : `Result${parentAgent ? ` · returned to ${parentAgent}` : ""}`}</h4>
        {json !== null && <CopyButton json={json} />}
      </div>
      {summary && <p>{summary}</p>}
      {json !== null && (
        <details className="result-json group" open={lines <= COLLAPSE_OVER_LINES}>
          <summary>
            <span className="text-muted-foreground [transition:transform_150ms_ease] group-open:rotate-90" aria-hidden>
              ▸
            </span>
            payload · {lines.toLocaleString("en-US")} line{lines === 1 ? "" : "s"}
          </summary>
          <pre>{json}</pre>
        </details>
      )}
    </section>
  );
}

// CopyButton is the panel's copy control (.result .copy): it sits in the
// panel's header row, beside the heading, and confirms itself — "Copied" for
// a beat — rather than making the reader wonder whether anything
// happened. A clipboard that refuses (a non-secure context) just leaves the
// button at its label; the JSON is right there to select by hand.
function CopyButton({ json }: { json: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <Button
      variant="ghost"
      size="sm"
      className="copy"
      onClick={() => {
        navigator.clipboard?.writeText(json).then(
          () => {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1500);
          },
          () => {},
        );
      }}
    >
      {copied ? "Copied" : "Copy JSON"}
    </Button>
  );
}
