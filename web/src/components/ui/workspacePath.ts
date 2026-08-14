// Every path a session touches lives under its own workspace directory, and
// that directory's parent is the same string on every row of every run — the
// workspace root. Since path parity (docs/WORKTREES.md) the root is a real
// host path rather than a container-only /workspaces, so it is both longer
// and more personal: it carries the operator's home directory and the name
// of whatever checkout the stack was started from. Printed whole it spends
// the first fifty characters of every line saying what the Session panel
// already says, and pushes the part that differs off the end.
//
// splitWorkspacePath cuts a path into the three parts the display treats
// differently: the root (identical everywhere, worth hiding), the session
// directory (identifies the run) and the rest (the path as the model would
// have written it inside the repo).
//
// The session directory is the anchor rather than the root, because the root
// is configurable — HARNESS_WORKSPACES — and older sessions recorded theirs
// under the pre-parity /workspaces. Anchoring on the "sess-" segment splits
// both without the browser needing to know which root produced the path.
export interface WorkspacePath {
  // Trailing slash included, so the parts concatenate back to the original.
  root: string;
  session: string;
  // "" when the path is the workspace directory itself.
  rest: string;
}

const SESSION_SEGMENT = /^(.*\/)(sess-[^/]+)(?:\/(.*))?$/;

// splitWorkspacePath returns null for a path with no session directory in it
// — an absolute path elsewhere on the box, where the leading slash is the
// fact and there is no shared prefix to hide.
export function splitWorkspacePath(path: string): WorkspacePath | null {
  const m = SESSION_SEGMENT.exec(path);
  if (!m) return null;
  return { root: m[1], session: m[2], rest: m[3] ?? "" };
}

// hiddenPrefix and visibleTail are the two halves an elided path renders,
// split at the point that depends on what the reader needs.
//
// keepSession is the difference between the two places a workspace path is
// shown. A tool call's target wants neither root nor session directory: the
// screen is already about this one session, so "internal/fold/fold.go" is the
// whole of what distinguishes that row. The Session panel's workspace fact
// wants the session directory kept, because there the directory name is the
// fact being stated.
// A workspace path embedded in prose — the "Workspace: …" line the harness
// opens every run with. Anything up to the session directory is the root;
// quotes and backticks end the match so a path inside a code span is not
// swallowed along with its delimiter.
const EMBEDDED = /\/[^\s`'"]*?\/(?=sess-)/g;

// shortenWorkspacePaths drops the root from every workspace path in a line of
// text, keeping the session directory. For summary lines that are prose
// rather than a single path value, and so cannot carry an ElidedPath control
// — the block they summarise discloses the text in full, which is where the
// whole path remains available.
export function shortenWorkspacePaths(text: string): string {
  return text.replace(EMBEDDED, "…/");
}

export function elidePath(split: WorkspacePath, keepSession: boolean): { hidden: string; visible: string } {
  if (keepSession) return { hidden: split.root, visible: split.session + (split.rest ? `/${split.rest}` : "") };
  const hidden = split.root + split.session + (split.rest ? "/" : "");
  // The workspace directory itself, named rather than left as an empty
  // string — a row that elided to nothing would read as a missing value.
  return { hidden, visible: split.rest || "workspace root" };
}
