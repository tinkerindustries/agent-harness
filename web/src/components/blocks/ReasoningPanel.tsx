// ReasoningPanel is voluminous and mostly skimmed (docs/DESIGN.md §5.6): it
// expands while streaming and collapses on completion, showing elapsed time
// and an approximate token count. The count is a character-based estimate,
// not the API's own reasoning_tokens figure — that number belongs to the
// usage event, arrives after the block that would show it has already
// frozen, and is phase 6's territory (per-turn cost accounting) rather than
// this panel's.
function approxTokens(text: string): number {
  return Math.max(1, Math.round(text.length / 4));
}

function formatElapsed(ms: number): string {
  if (ms < 0 || !Number.isFinite(ms)) return "";
  const s = ms / 1000;
  return s < 10 ? `${s.toFixed(1)}s` : `${Math.round(s)}s`;
}

interface Props {
  text: string;
  defaultOpen: boolean;
  elapsedMs?: number;
  startedAt?: string;
}

export function ReasoningPanel({ text, defaultOpen, elapsedMs, startedAt }: Props) {
  const elapsed = elapsedMs ?? (startedAt ? Date.now() - Date.parse(startedAt) : undefined);
  const elapsedLabel = elapsed !== undefined ? formatElapsed(elapsed) : "";
  return (
    <details className="reasoning" open={defaultOpen}>
      <summary>
        reasoning{elapsedLabel && ` · ${elapsedLabel}`} · ~{approxTokens(text)} tokens
      </summary>
      <pre className="block-text reasoning-text">{text}</pre>
    </details>
  );
}
