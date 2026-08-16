import { useMemo, useState } from "react";
import { CodeBlock } from "../../render/CodeBlock";
import { Button } from "../ui/button";

// A 5,000-line file read is a disclosure problem, not a virtualisation
// problem (docs/DESIGN.md §5.4): collapse to a head and tail preview with an
// expand control instead of rendering (or virtualising) the whole thing.
const COLLAPSE_LINE_THRESHOLD = 40;
const HEAD_LINES = 20;
const TAIL_LINES = 10;

interface Props {
  text: string;
  language?: string;
}

export function CollapsibleOutput({ text, language }: Props) {
  const [expanded, setExpanded] = useState(false);
  const lines = useMemo(() => text.split("\n"), [text]);

  if (lines.length <= COLLAPSE_LINE_THRESHOLD) {
    return <CodeBlock code={text} language={language} />;
  }
  if (expanded) {
    return (
      <>
        <CodeBlock code={text} language={language} />
        <Button variant="outline" size="sm" className="text-[var(--status-running)]" onClick={() => setExpanded(false)}>
          Show less
        </Button>
      </>
    );
  }

  const head = lines.slice(0, HEAD_LINES).join("\n");
  const tail = lines.slice(-TAIL_LINES).join("\n");
  const hidden = lines.length - HEAD_LINES - TAIL_LINES;
  return (
    <>
      <CodeBlock code={head} language={language} />
      <Button variant="outline" size="sm" className="text-[var(--status-running)]" onClick={() => setExpanded(true)}>
        Show {hidden} more lines
      </Button>
      <CodeBlock code={tail} language={language} />
    </>
  );
}
