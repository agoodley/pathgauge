// Settings and the burst model of the CDP replication tests (cdp-up,
// cdp-down). Mirrors CDPModel.WithDefaults and Rates in
// internal/flow/cdp.go and CDPDefaults in internal/engine/cdp.go; keep
// them in step.
import type { CDPSettings } from './types';

export const CDP_TESTS = ['cdp-up', 'cdp-down'];

/** Defaults the engine fills in for unset (zero) settings. */
export const CDP_DEFAULTS = { burst_factor: 3, burst_s: 10, period_s: 60, streams: 4, rpo_s: 15 };

/** Rate without a TCP test to take half of. */
export const CDP_FALLBACK_RATE = 20;

/** Most TCP connections the endpoint accepts. */
export const CDP_MAX_STREAMS = 128;

/** Default duration: 60 s × scale, at least 20 s. */
export function cdpDefaultDuration(scale: number): number {
  return Math.max(20, 60 * (scale > 0 ? scale : 1));
}

export type CDPKey = keyof CDPSettings;

export interface CDPField {
  key: CDPKey;
  label: string;
  hint: string;
  /** Whole numbers only (connections). */
  integer?: boolean;
  /** Smallest value accepted (0 always means the default). */
  min?: number;
  max?: number;
}

/** Form fields in display order. */
export const CDP_FIELDS: CDPField[] = [
  {
    key: 'rate_mbps',
    label: 'Change rate (Mbit/s)',
    hint: 'Average, on the wire after compression (1 MB/s = 8 Mbit/s). Empty: half the measured TCP rate, or 20.',
  },
  { key: 'burst_factor', label: 'Burst factor (× average)', hint: 'Write bursts as a multiple of the average rate.', min: 1 },
  { key: 'burst_s', label: 'Burst length (s)', hint: 'How long each write burst lasts.' },
  { key: 'period_s', label: 'Burst period (s)', hint: 'Time from one burst to the next.' },
  { key: 'limit_mbps', label: 'Throttle (Mbit/s)', hint: 'Cap replication, like a Veeam network traffic rule.' },
  { key: 'streams', label: 'TCP connections', hint: `Parallel replication streams (1–${CDP_MAX_STREAMS}).`, integer: true, max: CDP_MAX_STREAMS },
  { key: 'rpo_s', label: 'Target RPO (s)', hint: 'The lag to check against (Veeam’s default is 15 s).' },
  { key: 'duration_s', label: 'Duration (s)', hint: 'Run for several burst periods for a sizing answer.' },
];

export type CDPRaw = Partial<Record<CDPKey, string>>;

/** Burst shape after the engine's defaults and clamps. */
export interface CDPShape {
  factor: number;
  burstS: number;
  periodS: number;
  /** The burst factor (or length) asked for was cut to fit the period. */
  factorClamped: boolean;
  burstClamped: boolean;
}

/** Same defaults and clamps as CDPModel.WithDefaults. */
export function cdpShape(s: CDPSettings): CDPShape {
  let factor = s.burst_factor ?? 0;
  if (!(factor >= 1)) factor = CDP_DEFAULTS.burst_factor;
  let burstS = (s.burst_s ?? 0) > 0 ? (s.burst_s as number) : CDP_DEFAULTS.burst_s;
  const periodS = (s.period_s ?? 0) > 0 ? (s.period_s as number) : CDP_DEFAULTS.period_s;
  const burstClamped = burstS > periodS;
  if (burstClamped) burstS = periodS;
  const factorClamped = factor > periodS / burstS;
  if (factorClamped) factor = periodS / burstS;
  return { factor, burstS, periodS, factorClamped, burstClamped };
}

/**
 * Rates between and during bursts (Mbit/s) for an average rate, as
 * CDPModel.Rates: peak = average × factor, and the rest of the period
 * makes up the average.
 */
export function cdpRates(avgMbps: number, sh: CDPShape): { base: number; peak: number } {
  const peak = avgMbps * sh.factor;
  if (sh.periodS === sh.burstS) return { base: peak, peak };
  const base = (avgMbps * sh.periodS - peak * sh.burstS) / (sh.periodS - sh.burstS);
  return { base: Math.max(0, base), peak };
}

/** Validate the form: settings to send (only the filled fields) or an error. */
export function validateCDP(raw: CDPRaw): { settings: CDPSettings; error: string | null; bad: Set<CDPKey> } {
  const settings: CDPSettings = {};
  const bad = new Set<CDPKey>();
  const errors: string[] = [];
  for (const f of CDP_FIELDS) {
    const v = (raw[f.key] ?? '').trim();
    if (!v) continue;
    const n = Number(v);
    const name = f.label.replace(/ \(.*\)$/, '');
    let err = '';
    if (!Number.isFinite(n) || n < 0) err = `${name} must be a positive number.`;
    else if (f.integer && !Number.isInteger(n)) err = `${name} must be a whole number.`;
    else if (n > 0 && f.min !== undefined && n < f.min) err = `${name} must be at least ${f.min}.`;
    else if (f.max !== undefined && n > f.max) err = `${name}: at most ${f.max}.`;
    if (err) {
      bad.add(f.key);
      errors.push(err);
      continue;
    }
    // Zero means the default, as in the engine: leave it out.
    if (n > 0) settings[f.key] = n;
  }
  return { settings, error: errors.length ? errors.join(' ') : null, bad };
}

/** Settings as a URL parameter: "rate_mbps:200,burst_s:3". */
export function formatCDP(s: CDPSettings): string {
  return CDP_FIELDS.filter((f) => (s[f.key] ?? 0) > 0)
    .map((f) => `${f.key}:${s[f.key]}`)
    .join(',');
}

/** Form values from a "rate_mbps:200,burst_s:3" URL parameter. */
export function parseCDP(q: string | null): CDPRaw {
  const raw: CDPRaw = {};
  for (const part of (q ?? '').split(',')) {
    const [k, v] = part.split(':').map((x) => x.trim());
    const f = CDP_FIELDS.find((x) => x.key === k);
    if (f && v && Number.isFinite(Number(v))) raw[f.key] = v;
  }
  return raw;
}
