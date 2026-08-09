import DOMPurify from "dompurify";
import { marked } from "marked";
import { useEffect, useMemo, useRef } from "react";
import hljs from "./highlight";

marked.setOptions({ gfm: true, breaks: false });

interface Props {
  text: string;
}

// Markdown renders assistant prose on completion, not while it streams
// (docs/DESIGN.md §5.3): marked.parse runs once, memoised on the block's own
// immutable text, and highlight.js runs once more over the result after it
// mounts. The text originates from the model, which can itself be quoting
// untrusted material it read off disk or fetched from the web, so the
// parsed HTML always goes through DOMPurify before it reaches
// dangerouslySetInnerHTML.
export function Markdown({ text }: Props) {
  const html = useMemo(() => DOMPurify.sanitize(marked.parse(text, { async: false }) as string), [text]);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    ref.current?.querySelectorAll("pre code").forEach((el) => hljs.highlightElement(el as HTMLElement));
  }, [html]);

  return <div className="markdown" ref={ref} dangerouslySetInnerHTML={{ __html: html }} />;
}
