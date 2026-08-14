import type { SessionState } from "../api/types";

// The session list's title/description rendering, as a pure function of the
// wire row — the same shape the other display helpers take (statusBadge).
// The main page shows the run's title bold with the description
// beneath it, and the raw prompt only as a fallback: a pre-migration row or a
// browser start with no title renders no bold line at all and uses the task
// as the description line, so no row ever goes blank.

export interface TitleLines {
  // The bold title line, rendered in the .sess-title span (font-weight 600)
  // with the phase chip beside it. null renders no bold title.
  title: string | null;
  // The line under the title: the run's own description, or the raw prompt
  // when there is no title (or no description) to show.
  desc: string;
  // The "phase N/M" chip text, null when the run is not part of a chain
  // (total_phases absent or 0).
  phase: string | null;
}

export function titleLines(sess: SessionState): TitleLines {
  const phase =
    sess.total_phases && sess.total_phases > 0
      ? `phase ${sess.phase ?? 0}/${sess.total_phases}`
      : null;
  return {
    title: sess.title || null,
    desc: sess.description || sess.task || "",
    phase,
  };
}
