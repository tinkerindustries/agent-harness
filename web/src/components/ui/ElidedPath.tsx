import { useMemo, useState } from "react";
import { cn } from "@/lib/utils";
import { elidePath, splitWorkspacePath } from "./workspacePath";

// ElidedPath renders a workspace path with its shared prefix collapsed
// behind a control, rather than dropping the prefix silently. The prefix is
// the same on every row and carries the operator's own directory names
// (docs/WORKTREES.md, "Path parity" — the root is a real host path), so
// hiding it is what makes a dense row readable and a shared screen safe. But
// a path that has been shortened without saying so is a path the reader
// cannot trust, and the full string is occasionally the thing they need —
// to paste into a terminal, or to check which root a run actually used. So
// the elision is a button, and clicking it puts the whole path back.
//
// The prefix is revealed on click only, never on hover: the title says what
// the control does rather than naming the path, so passing the cursor over a
// row during a screen share cannot expose it.
export function ElidedPath({
  path,
  keepSession = false,
  className,
}: {
  path: string;
  // Whether the session directory stays visible — see elidePath.
  keepSession?: boolean;
  className?: string;
}) {
  const [expanded, setExpanded] = useState(false);
  const split = useMemo(() => splitWorkspacePath(path), [path]);
  // A path with no workspace prefix has nothing to hide, and the control
  // would be an affordance that does nothing.
  if (!split) return <span className={className}>{path}</span>;
  const { visible } = elidePath(split, keepSession);
  return (
    // path-full releases the truncation the surrounding row applies. The
    // rows these sit in ellipsise their values, which is right for a
    // collapsed path and defeats the whole control for an expanded one —
    // clicking to see the path and getting a shorter path back.
    <span className={cn(className, expanded && "path-full")}>
      <button
        type="button"
        className="path-elide"
        aria-expanded={expanded}
        title={expanded ? "Shorten the path" : "Show the full path"}
        aria-label={expanded ? "Shorten the path" : "Show the full path"}
        // These sit inside rows that are themselves clickable (a session
        // card opens the session), so the toggle claims its own click.
        onClick={(e) => {
          e.stopPropagation();
          e.preventDefault();
          setExpanded((v) => !v);
        }}
      >
        {expanded ? "⌄" : "⋯/"}
      </button>
      {/* Expanded renders the original string rather than the two halves
          rejoined, so what can be selected and copied is exactly the path
          the harness recorded. */}
      {expanded ? path : visible}
    </span>
  );
}
