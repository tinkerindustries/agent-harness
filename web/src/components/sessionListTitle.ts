import type { SessionListRow } from "../api/types";

// The session list's title/description rendering, as a pure function of the
// wire row — the same shape the other display helpers take (statusBadge).
// The main page shows the run's title bold with the description
// beneath it. The description line shows the description when there is one,
// and nothing for a run that has a title but no description — the raw prompt
// is not repeated under a title that says what the run is. Only a run with
// neither a title nor a description — a pre-migration row or a blank browser
// start — falls back to the raw prompt, so no row ever goes blank.

export interface TitleLines {
  // The bold title line, rendered in the .sess-title span (font-weight 600)
  // with the phase chip beside it. null renders no bold title.
  title: string | null;
  // The line under the title: the run's own description; "" when the run has
  // a title but no description, so the render sites skip the line; and the
  // raw prompt when there is no title at all, so no row ever goes blank.
  desc: string;
  // The "phase N/M" chip text, null when the run is not part of a chain
  // (total_phases absent or 0).
  phase: string | null;
}

export function titleLines(sess: SessionListRow): TitleLines {
  const phase =
    sess.total_phases && sess.total_phases > 0
      ? `phase ${sess.phase ?? 0}/${sess.total_phases}`
      : null;
  return {
    title: sess.title || null,
    desc: descriptionLine(sess),
    phase,
  };
}

// descriptionLine picks the description line under the title: the run's own
// description when there is one, nothing when the title already says what the
// run is, and the raw prompt only when there is no title to say it.
function descriptionLine(sess: SessionListRow): string {
  if (sess.description) return sess.description;
  if (sess.title) return "";
  return sess.task || "";
}
