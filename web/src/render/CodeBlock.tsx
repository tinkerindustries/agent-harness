import { useEffect, useRef } from "react";
import hljs from "./highlight";

interface Props {
  code: string;
  language?: string;
}

// CodeBlock highlights once, after mount, rather than on every render — "on
// completion it parses and highlights once, then swaps in" (docs/DESIGN.md
// §5.3). It is only ever mounted inside an already-frozen block behind
// React.memo, so this effect runs exactly once for that block's lifetime;
// nothing here re-runs on the deltas that built the text in the first
// place, because streaming text never reaches this component at all (see
// LiveAssistantBlock and LivePendingToolBlock, which render the same text
// as plain preformatted content while it is still growing).
export function CodeBlock({ code, language }: Props) {
  const ref = useRef<HTMLElement>(null);

  useEffect(() => {
    if (ref.current) hljs.highlightElement(ref.current);
    // Empty deps: code/language come from an immutable block and never
    // change for the lifetime of this component instance.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <pre className="code-block">
      <code ref={ref} className={language ? `language-${language}` : undefined}>
        {code}
      </code>
    </pre>
  );
}
