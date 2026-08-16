import { BLOCK_TEXT_CLS } from "./blockStyles";
import { cn } from "@/lib/utils";

// ReasoningPanel is voluminous and mostly skimmed (docs/DESIGN.md §5.6): it
// expands while streaming and collapses on completion, showing elapsed time
// and a token count. Completed sub-turns show the API's own reasoning_tokens
// figure, attached by the fold when the usage event lands; a sub-turn still
// streaming has no usage event yet, so its count falls back to a
// character-based estimate.
function approxTokens(text: string): number {
  return Math.max(1, Math.round(text.length / 4));
}

// formatElapsed is shared with SubTurnCard, which shows the same wall-clock
// figure in the sub-turn header.
export function formatElapsed(ms: number): string {
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
    // .reasoning's own CSS never styled the <details> itself, only its
    // <summary> (a plain descendant selector, not a pseudo-element or a
    // Markdown-rendered child) — so unlike .think/.tool it needs no residual
    // class at all once that moves onto the <summary> directly.
    <details open={defaultOpen}>
      <summary className="cursor-pointer text-[0.85rem] text-muted-foreground">
        reasoning{elapsedLabel && ` · ${elapsedLabel}`} · {tokenLabel}
      </summary>
      <pre className={cn(BLOCK_TEXT_CLS, "text-[0.85rem] text-muted-foreground")}>{text}</pre>
    </details>
  );
}
