// Frame-time statistics for the measurement harness. A frame delta
// is the time between two consecutive requestAnimationFrame callbacks — the
// number that answers "did this hold 60fps," independent of how the store
// itself batches updates.
export interface FrameStats {
  frames: number;
  meanMs: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
  maxMs: number;
  over16Count: number; // missed the 60fps (16.7ms) budget
  over33Count: number; // missed the 30fps (33.4ms) budget
}

const ZERO: FrameStats = { frames: 0, meanMs: 0, p50Ms: 0, p95Ms: 0, p99Ms: 0, maxMs: 0, over16Count: 0, over33Count: 0 };

export function computeFrameStats(deltas: number[]): FrameStats {
  if (deltas.length === 0) return ZERO;
  const sorted = [...deltas].sort((a, b) => a - b);
  const pct = (p: number) => sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))];
  const sum = deltas.reduce((a, b) => a + b, 0);
  return {
    frames: deltas.length,
    meanMs: sum / deltas.length,
    p50Ms: pct(50),
    p95Ms: pct(95),
    p99Ms: pct(99),
    maxMs: sorted[sorted.length - 1],
    over16Count: deltas.filter((d) => d > 16.7).length,
    over33Count: deltas.filter((d) => d > 33.4).length,
  };
}
