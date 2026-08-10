// ReasoningPanel is voluminous and mostly skimmed (docs/DESIGN.md §5.6): it
// expands while streaming and collapses on completion, showing elapsed time
// and a token count. Completed sub-turns show the API's own reasoning_tokens
// figure, attached by the fold when the usage event lands; a sub-turn still
// streaming has no usage event yet, so its count falls back to a
// character-based estimate.
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
  tokens?: number;
}

export function ReasoningPanel({ text, defaultOpen, elapsedMs, startedAt, tokens }: Props) {
  const elapsed = elapsedMs ?? (startedAt ? Date.now() - Date.parse(startedAt) : undefined);
  const elapsedLabel = elapsed !== undefined ? formatElapsed(elapsed) : "";
  const tokenLabel = tokens !== undefined ? `${tokens} tokens` : `~${approxTokens(text)} tokens`;
  return (
    <details className="reasoning" open={defaultOpen}>
      <summary>
        reasoning{elapsedLabel && ` · ${elapsedLabel}`} · {tokenLabel}
      </summary>
      <pre className="block-text reasoning-text">{text}</pre>
    </details>
  );
}
