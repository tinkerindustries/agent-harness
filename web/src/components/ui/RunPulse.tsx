import { useEffect, useRef } from "react";
import { BUCKETS, BUCKET_MS, WINDOW_MS, type PulseFrame, type PulseMeter } from "@/api/pulse";

// The run pulse: a canvas strip of how much this session has produced over
// the last ninety seconds, stacked as reasoning, prose and tool output, with
// a mark where each tool call started and where one failed.
//
// It answers the one question the footer's "Thinking · 1m 04s" cannot. That
// line says what the run is doing and how long it has been doing it; it says
// nothing about whether anything is coming out, so a model streaming steadily
// and a model wedged mid-request read identically. The strip is the
// difference between them.
//
// Canvas rather than DOM, and outside React entirely. The input is the delta
// path — the one thing in this app that moves at token rate — and every rule
// in web/CLAUDE.md's frame-budget section exists to keep that path from
// reaching a React commit. A hundred and eighty bars redrawn per frame is a
// few hundred microseconds of fill; the same thing as elements would be a
// hundred and eighty nodes reconciled sixty times a second, which is the
// exact shape of update those rules forbid. So the meter is read here, on
// this component's own animation frame, and never enters a snapshot as a
// value (api/pulse.ts).
//
// The component takes the meter as a prop and never re-renders on the data:
// the effect below is keyed on the meter reference, which is stable for a
// session's whole life, so React mounts this once per session and then has
// nothing further to do with it.

// A bar per bucket with a hairline gap, so the strip reads as samples rather
// than as a continuous area.
const BAR_GAP = 1;

// The stacked total is drawn on a log scale. The bands span orders of
// magnitude — a build's stdout is thousands of characters in a bucket where
// a reasoning stream is a couple of hundred — and on a linear scale one noisy
// Bash call flattens every model band in the window to a single pixel. The
// log is applied to the bucket's total and the bands take their share of the
// resulting height, so the proportions within a bar stay true while the bars
// stay comparable to each other.
const scaleOf = (total: number, peak: number) => (peak <= 0 ? 0 : Math.log1p(total) / Math.log1p(peak));

// The peak decays rather than being recomputed from the window, so the strip
// does not re-scale itself every time the loudest bar scrolls off the left
// edge — a step change in every bar's height reads as activity that did not
// happen. 0.995 per frame is a little under a second to fall by a quarter;
// FRAME_MS is what "per frame" means now that draws are not every frame.
const PEAK_DECAY = 0.995;
const FRAME_MS = 1000 / 60;
// Below this the scale stops shrinking, so a run producing almost nothing
// does not amplify its own noise into a full-height strip.
const MIN_PEAK = 400;

interface Palette {
  reasoning: string;
  content: string;
  stdout: string;
  tool: string;
  error: string;
  rule: string;
}

function readPalette(el: HTMLElement): Palette {
  const s = getComputedStyle(el);
  const v = (name: string, fallback: string) => s.getPropertyValue(name).trim() || fallback;
  return {
    // Reasoning borrows the rail's read tick, the one hue the palette does
    // not otherwise spend; prose takes the live accent, because prose landing
    // is the run talking. Tool output is deliberately the dimmest of the
    // three: it is the loudest by volume and the least informative.
    reasoning: v("--tick-read", "#8b5cf6"),
    content: v("--accent-live", "#2563eb"),
    stdout: v("--text-dim", "#71717a"),
    // The notch borrows the dim text colour rather than the border one: a
    // mark drawn at hairline strength on a strip this short is a mark nobody
    // sees.
    tool: v("--text-dim", "#71717a"),
    error: v("--accent-bad", "#dc2626"),
    rule: v("--line-hairline", "#e4e4e7"),
  };
}

export function RunPulse({ meter, className }: { meter: PulseMeter; className?: string }) {
  const ref = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const canvas = ref.current;
    if (!canvas) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    let palette = readPalette(canvas);
    let width = 0;
    let height = 0;
    let peak = MIN_PEAK;
    // The meter revision the canvas currently shows, and when it was drawn.
    // -1 forces the first pass.
    let shownRev = -1;
    let lastDrawAt = 0;

    const resize = () => {
      const dpr = window.devicePixelRatio || 1;
      const rect = canvas.getBoundingClientRect();
      width = Math.max(1, Math.round(rect.width));
      height = Math.max(1, Math.round(rect.height));
      canvas.width = Math.round(width * dpr);
      canvas.height = Math.round(height * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };

    const draw = () => {
      const now = Date.now();
      const frame: PulseFrame = meter.read(now);
      shownRev = meter.revision;
      // The peak decays with elapsed time rather than per draw. Now that a
      // draw only happens when something changed, "per draw" would make the
      // scale fall fastest exactly when the run is busiest — which is
      // backwards, and would have been an invisible bug: the strip would
      // still look plausible.
      const sinceMs = lastDrawAt === 0 ? 0 : now - lastDrawAt;
      lastDrawAt = now;
      ctx.clearRect(0, 0, width, height);

      // The baseline is drawn whatever the data: an empty strip with no line
      // in it reads as a component that failed to render, and the first
      // seconds of a page opened mid-run are legitimately empty — this meter
      // only ever holds what arrived while somebody was watching.
      ctx.fillStyle = palette.rule;
      ctx.globalAlpha = 1;
      ctx.fillRect(0, height - 1, width, 1);

      const barW = width / BUCKETS;
      const drawW = Math.max(0.5, barW - BAR_GAP);
      // Marks sit in a lane below the bands so a tool call starting never
      // adds height to a bar and pretends to be output. Four pixels of it,
      // because at three the notch was there in the DOM and invisible on the
      // strip — which is the failure mode this whole component is one long
      // argument against.
      const laneH = 4;
      const plotH = height - laneH - 1;

      let observed = 0;
      for (let i = 0; i < BUCKETS; i++) {
        const total = frame.reasoning[i] + frame.content[i] + frame.stdout[i];
        if (total > observed) observed = total;
      }
      const decayed = peak * Math.pow(PEAK_DECAY, sinceMs / FRAME_MS);
      peak = Math.max(observed, decayed, MIN_PEAK);

      for (let i = 0; i < BUCKETS; i++) {
        const x = i * barW;
        const r = frame.reasoning[i];
        const c = frame.content[i];
        const o = frame.stdout[i];
        const total = r + c + o;

        if (total > 0) {
          const h = scaleOf(total, peak) * plotH;
          // Each band takes its share of the bar's height. Sub-pixel bands
          // are rounded up to a visible sliver rather than away: a bucket
          // with three characters of prose in it is the difference between a
          // run that is moving and one that is not.
          let y = plotH;
          const bands: [number, string][] = [
            [o, palette.stdout],
            [r, palette.reasoning],
            [c, palette.content],
          ];
          for (const [value, colour] of bands) {
            if (value <= 0) continue;
            const bandH = Math.max(0.75, (value / total) * h);
            y -= bandH;
            ctx.fillStyle = colour;
            ctx.globalAlpha = colour === palette.stdout ? 0.45 : 0.9;
            ctx.fillRect(x, y, drawW, bandH);
          }
        }

        if (frame.tools[i] > 0) {
          ctx.fillStyle = palette.tool;
          ctx.globalAlpha = 1;
          ctx.fillRect(x, height - laneH, Math.max(1.5, drawW), laneH - 1);
        }
        if (frame.errors[i] > 0) {
          // An error is the one thing here worth interrupting the shape for,
          // so it is the full height of the plot rather than a mark in the
          // lane.
          ctx.fillStyle = palette.error;
          ctx.globalAlpha = 0.85;
          ctx.fillRect(x, 0, Math.max(1, drawW), height - 1);
        }
      }
      ctx.globalAlpha = 1;
    };

    resize();
    draw();

    const ro = new ResizeObserver(() => {
      resize();
      draw();
    });
    ro.observe(canvas);

    // The palette is re-read on a theme change rather than every frame: it is
    // six getComputedStyle reads, which is a style recalculation, and doing
    // that inside an animation frame loop is how a decorative strip becomes
    // the most expensive thing on the page.
    const repaint = () => {
      palette = readPalette(canvas);
      draw();
    };
    const themeAttr = new MutationObserver(repaint);
    themeAttr.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "class"] });
    const scheme = window.matchMedia("(prefers-color-scheme: dark)");
    scheme.addEventListener("change", repaint);

    // tick is the animation frame's whole body: draw if the picture would
    // differ, and otherwise do nothing at all. Two things can make it differ
    // — the meter changed (a delta, a tool call), or enough time has passed
    // that the window has scrolled on a bucket. The second cannot be read off
    // the revision, because a meter nobody is writing to does not advance
    // until somebody reads it, so an idle run would sit on a stale picture
    // for ever waiting for a change that only the read would cause.
    const tick = () => {
      if (meter.revision !== shownRev || Date.now() - lastDrawAt >= BUCKET_MS) draw();
    };

    // Reduced motion keeps the data and drops the animation: the strip is a
    // reading, not a flourish, so it still updates — once a second, which is
    // the cadence of every other figure in this footer, rather than at frame
    // rate.
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)");
    let raf = 0;
    let timer = 0;
    const start = () => {
      if (reduced.matches) {
        timer = window.setInterval(draw, 1000);
      } else {
        const loop = () => {
          tick();
          raf = requestAnimationFrame(loop);
        };
        raf = requestAnimationFrame(loop);
      }
    };
    const stop = () => {
      if (raf) cancelAnimationFrame(raf);
      if (timer) window.clearInterval(timer);
      raf = 0;
      timer = 0;
    };
    const restart = () => {
      stop();
      start();
    };
    reduced.addEventListener("change", restart);
    start();

    return () => {
      stop();
      reduced.removeEventListener("change", restart);
      scheme.removeEventListener("change", repaint);
      themeAttr.disconnect();
      ro.disconnect();
    };
  }, [meter]);

  return (
    <canvas
      ref={ref}
      className={className}
      // The shape is not readable without sight and the numbers behind it are
      // already on the status line beside it, in words, ticking. So the strip
      // states what it is and does not try to narrate itself.
      role="img"
      aria-label={`Run pulse: model and tool output over the last ${Math.round(WINDOW_MS / 1000)} seconds, in ${BUCKET_MS}ms samples`}
    />
  );
}
