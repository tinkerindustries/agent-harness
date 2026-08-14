// The rate schedule, and what a screen can say about it.
//
// The browser prices nothing. Every cost figure in this app was computed on
// the server when its usage event was committed and stored on that event
// (docs/DESIGN.md §4.9), which is what keeps a historical figure stable —
// so `GET /api/pricing` carries no rates at all, only *when* the expensive
// hours are. There is nothing here to compute a cost with, on purpose.
//
// What is here is the half the server cannot do: rendering those hours where
// the person reading is sitting. The windows arrive in UTC because that is
// how DeepSeek states them and what they bill on; the server has no idea what
// zone its reader is in and the browser knows exactly, so the conversion
// belongs on this side and only on this side.

export interface PricingWindow {
  /** "HH:MM", UTC. */
  from: string;
  to: string;
}

export interface PricingSchedule {
  /** RFC3339 UTC. Before this instant every model bills a flat rate. */
  effective_at: string;
  peak_windows_utc: PricingWindow[];
  // The models priced by the hour. A model absent from this list bills one
  // rate around the clock — every Gemini and Kimi entry — so a screen showing
  // one of those sessions must not tell its reader the hour matters.
  models: string[];
}

export interface Pricing {
  captured_at: string;
  schedule?: PricingSchedule;
}

const MINUTES_PER_DAY = 24 * 60;

function toMinutes(hhmm: string): number | null {
  const m = /^(\d{1,2}):(\d{2})$/.exec(hhmm);
  if (!m) return null;
  const h = Number(m[1]);
  const min = Number(m[2]);
  if (h < 0 || h > 23 || min < 0 || min > 59) return null;
  return h * 60 + min;
}

// isPeakAt answers whether `at` falls in a peak window, in UTC, the same way
// internal/pricing does — half-open, so a window's `to` is outside it and two
// back-to-back windows never both claim the instant they meet.
//
// It also answers false before effective_at: the windows are real then but
// nothing is charged by them yet, and a screen that flagged peak rates a week
// early would be lying about a bill.
export function isPeakAt(schedule: PricingSchedule | undefined, at: Date): boolean {
  if (!schedule) return false;
  if (at.getTime() < Date.parse(schedule.effective_at)) return false;
  const minuteOfDay = at.getUTCHours() * 60 + at.getUTCMinutes();
  return schedule.peak_windows_utc.some((w) => {
    const from = toMinutes(w.from);
    const to = toMinutes(w.to);
    if (from === null || to === null || from === to) return false;
    return from < to ? minuteOfDay >= from && minuteOfDay < to : minuteOfDay >= from || minuteOfDay < to;
  });
}

// minutesUntilTierChange is how long the current tier lasts, in minutes: how
// long until peak ends, or until the next one starts. Null when there is no
// schedule, when it is not in force yet, or when the windows are unusable.
//
// This is what turns "peak rate" into something to act on. "It is expensive
// right now" invites a shrug; "expensive for another 40 minutes" is a
// decision about whether to start a run.
export function minutesUntilTierChange(schedule: PricingSchedule | undefined, at: Date): number | null {
  if (!schedule) return null;
  if (at.getTime() < Date.parse(schedule.effective_at)) return null;
  const bounds = schedule.peak_windows_utc
    .map((w) => ({ from: toMinutes(w.from), to: toMinutes(w.to) }))
    .filter((b): b is { from: number; to: number } => b.from !== null && b.to !== null && b.from !== b.to);
  if (bounds.length === 0) return null;

  const now = at.getUTCHours() * 60 + at.getUTCMinutes();
  // Every boundary in the day, as minutes from now, wrapping past midnight.
  // The nearest one is the next change whichever tier we are in, which is
  // why this does not need to know which that is.
  let best = MINUTES_PER_DAY;
  for (const b of bounds) {
    for (const edge of [b.from, b.to]) {
      const delta = (edge - now + MINUTES_PER_DAY) % MINUTES_PER_DAY;
      // A boundary exactly now is the one we just crossed, not the next.
      if (delta > 0 && delta < best) best = delta;
    }
  }
  return best === MINUTES_PER_DAY ? null : best;
}

export interface LocalWindow {
  from: string;
  to: string;
  /** The window ends on a later day than it starts, where the reader is. */
  crossed: boolean;
}

// localWindows renders the peak windows in the reader's own zone.
//
// This is the whole reason the schedule reaches the browser. "01:00-04:00
// UTC" is not a fact anybody acts on; at UTC+10 the same windows are
// 11:00-14:00 and 16:00-20:00 — the middle of a working day, which is a
// reason to start a long run this evening instead.
//
// `on` anchors the rendering to a date so a zone with daylight saving
// resolves to the offset actually in force then, rather than to whichever one
// today happens to have.
export function localWindows(schedule: PricingSchedule | undefined, on: Date = new Date()): LocalWindow[] {
  if (!schedule) return [];
  const out: LocalWindow[] = [];
  for (const w of schedule.peak_windows_utc) {
    const from = toMinutes(w.from);
    const to = toMinutes(w.to);
    if (from === null || to === null || from === to) continue;
    const start = new Date(Date.UTC(on.getUTCFullYear(), on.getUTCMonth(), on.getUTCDate(), Math.floor(from / 60), from % 60));
    // A window whose end is at or before its start wraps midnight in UTC.
    const endDay = to <= from ? on.getUTCDate() + 1 : on.getUTCDate();
    const end = new Date(Date.UTC(on.getUTCFullYear(), on.getUTCMonth(), endDay, Math.floor(to / 60), to % 60));
    out.push({
      from: hhmmLocal(start),
      to: hhmmLocal(end),
      crossed: start.getDate() !== end.getDate(),
    });
  }
  return out;
}

function hhmmLocal(d: Date): string {
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

// peakNote is the sentence a screen shows about the schedule, or "" when
// there is nothing worth saying.
//
// Nothing is said off-peak. A banner that reads "off-peak rate" whenever
// rates are normal is a banner people stop seeing, and by the time it says
// something else they have stopped looking — the same reason the CLI's tier
// note is silent on flat, and the reason the queue banner only appears when
// the pool halts.
export function peakNote(schedule: PricingSchedule | undefined, at: Date): string {
  if (!isPeakAt(schedule, at)) return "";
  const left = minutesUntilTierChange(schedule, at);
  if (left === null) return "peak rate";
  if (left < 60) return `peak rate for ${left}m`;
  const h = Math.floor(left / 60);
  const m = left % 60;
  return m === 0 ? `peak rate for ${h}h` : `peak rate for ${h}h ${m}m`;
}

// getPricing fetches the schedule once. It is config, not live data: the
// table changes when somebody redeploys, so this is a plain fetch on mount
// rather than anything on the SSE feed, and a failure leaves every caller
// showing nothing about peak hours — which is the same thing a harness with
// no schedule shows, and correct.
export async function getPricing(): Promise<Pricing | null> {
  try {
    const res = await fetch("/api/pricing");
    if (!res.ok) return null;
    return (await res.json()) as Pricing;
  } catch {
    return null;
  }
}
