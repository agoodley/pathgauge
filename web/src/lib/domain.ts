// Domain helpers: status classes, severities, duration estimates.
import { cdpDefaultDuration } from './cdp';
import { groupDefaultDuration, planSteps, type PlannedStep } from './groups';
import type { Agent, CheckStatus, Run } from './types';

export type StatusKind = 'good' | 'warning' | 'serious' | 'critical' | 'info' | 'neutral';

/** Health score bands (same thresholds as the HTML report). */
export function scoreStatus(s: number | null | undefined): { kind: StatusKind; icon: string; label: string } {
  if (s === null || s === undefined) return { kind: 'neutral', icon: '–', label: 'No score' };
  if (s >= 85) return { kind: 'good', icon: '✔', label: 'Good' };
  if (s >= 65) return { kind: 'warning', icon: '▲', label: 'Fair' };
  if (s >= 45) return { kind: 'serious', icon: '◆', label: 'Poor' };
  return { kind: 'critical', icon: '✖', label: 'Bad' };
}

export const severityInfo: Record<string, { kind: StatusKind; icon: string; label: string; rank: number }> = {
  critical: { kind: 'critical', icon: '✖', label: 'Critical', rank: 0 },
  warning: { kind: 'warning', icon: '▲', label: 'Warning', rank: 1 },
  info: { kind: 'info', icon: 'ℹ', label: 'Info', rank: 2 },
  good: { kind: 'good', icon: '✔', label: 'Good', rank: 3 },
};

export function severity(s: string) {
  return severityInfo[s] ?? { kind: 'neutral' as StatusKind, icon: '•', label: s || 'Unknown', rank: 9 };
}

export const checkInfo: Record<CheckStatus, { kind: StatusKind; icon: string; label: string }> = {
  ok: { kind: 'good', icon: '✔', label: 'OK' },
  warn: { kind: 'warning', icon: '▲', label: 'Warning' },
  missing: { kind: 'critical', icon: '✖', label: 'Missing' },
  info: { kind: 'info', icon: 'ℹ', label: 'Info' },
};

export function agentState(a: Pick<Agent, 'online' | 'busy' | 'revoked'>): { kind: StatusKind; label: string; icon: string } {
  if (a.revoked) return { kind: 'neutral', label: 'Revoked', icon: '⊘' };
  if (a.online && a.busy) return { kind: 'good', label: 'Online · testing', icon: '●' };
  if (a.online) return { kind: 'good', label: 'Online', icon: '●' };
  return { kind: 'critical', label: 'Offline', icon: '○' };
}

export function runStatusInfo(s: Run['status']): { kind: StatusKind; icon: string; label: string } {
  switch (s) {
    case 'done':
      return { kind: 'good', icon: '✔', label: 'Done' };
    case 'running':
      return { kind: 'info', icon: '◌', label: 'Running' };
    case 'queued':
      return { kind: 'info', icon: '…', label: 'Queued' };
    case 'failed':
      return { kind: 'critical', icon: '✖', label: 'Failed' };
    case 'cancelled':
      return { kind: 'neutral', icon: '⊘', label: 'Cancelled' };
  }
  return { kind: 'neutral', icon: '•', label: s };
}

export function gradeLabel(g: string): { kind: StatusKind; icon: string; label: string } {
  const k = gradeStatus(g);
  const m = { good: ['✔', 'Good'], warning: ['▲', 'Fair'], serious: ['◆', 'Poor'], critical: ['✖', 'Bad'] } as Record<string, [string, string]>;
  const [icon, label] = m[k] ?? ['•', 'Unknown'];
  return { kind: k, icon, label };
}

/**
 * Quality verdict of a simulated session, phase or capacity step (traffic
 * mix and call capacity). Transfers have none.
 */
export function verdictInfo(v: string | null | undefined): { kind: StatusKind; icon: string; label: string } {
  switch (v) {
    case 'good':
      return { kind: 'good', icon: '✔', label: 'Good' };
    case 'acceptable':
      return { kind: 'warning', icon: '▲', label: 'Acceptable' };
    case 'poor':
      return { kind: 'critical', icon: '✖', label: 'Poor' };
  }
  return { kind: 'neutral', icon: '–', label: 'No verdict' };
}

/** Grade order for comparisons: lower index is better. */
export const gradeOrder = ['A+', 'A', 'B', 'C', 'D', 'F'];

export function gradeStatus(g: string): StatusKind {
  const i = gradeOrder.indexOf(g);
  if (i < 0) return 'neutral';
  if (i <= 1) return 'good';
  if (i === 2) return 'warning';
  if (i <= 4) return 'serious';
  return 'critical';
}

// Approximate seconds per test at scale 1 (from internal/engine/runner.go
// durations plus setup). Fixed-time tests do not scale.
const baseSeconds: Record<string, [scaled: number, fixed: number]> = {
  info: [0, 2],
  latency: [15, 2],
  trace: [0, 30],
  mtr: [0, 90],
  'tcp-down': [10, 3],
  'tcp-up': [10, 3],
  'tcp-down-1': [10, 3],
  'tcp-up-1': [10, 3],
  'streams-down': [25, 5],
  'streams-up': [25, 5],
  'udp-down': [10, 2],
  'udp-up': [10, 2],
  bufferbloat: [35, 4],
  'sweep-down': [30, 3],
  'sweep-up': [30, 3],
  'sizesweep-up': [16, 2],
  'sizesweep-down': [16, 2],
  pmtu: [0, 20],
  integrity: [0, 15],
  voip: [15, 2],
  'voip-loaded': [15, 3],
  cc: [32, 3],
  shortflow: [0, 15],
  discrimination: [36, 3],
  sqm: [120, 30],
  qos: [25, 4],
  // Mix: one or two phases of max(30 s × scale, 10 s). Capacity: two
  // searches of 6–9 steps of max(8 s × scale, 4 s) each, so the fixed
  // part is large.
  mix: [60, 10],
  'capacity-voice': [120, 50],
  'capacity-video': [100, 40],
};

// In a group every timed phase of a test lasts the group duration, so tests
// that run in phases take that many times as long: stream counts, the idle
// and three loaded bufferbloat phases, sweep steps, packet sizes, the
// congestion-control algorithms (typically 2–3), the throttling checks,
// QoS idle and loaded, the traffic mix with and without transfers.
const groupPhases: Record<string, number> = {
  'streams-down': 5,
  'streams-up': 5,
  bufferbloat: 4,
  'sweep-down': 10,
  'sweep-up': 10,
  'sizesweep-up': 7,
  'sizesweep-down': 7,
  cc: 3,
  discrimination: 3,
  qos: 2,
  mix: 2,
};

export interface EstimateOpts {
  /** Duration set for the CDP tests (empty: 60 s × scale, at least 20 s). */
  cdpDurationS?: number;
  /** Duration of each grouped test (empty: 30 s × scale, at least 15 s). */
  groupDurationS?: number;
  /** Grouped tests run on their own first (default true). */
  baseline?: boolean;
}

function testSeconds(t: string, s: number, o: EstimateOpts): number {
  if (t === 'cdp-up' || t === 'cdp-down') return (o.cdpDurationS && o.cdpDurationS > 0 ? o.cdpDurationS : cdpDefaultDuration(s)) + 5;
  const [a, b] = baseSeconds[t] ?? [10, 2];
  return a * s + b + 1;
}

/** One test in a group, every timed phase lasting d seconds. */
export function groupedTestSeconds(t: string, d: number): number {
  if (t === 'cdp-up' || t === 'cdp-down') return d + 5;
  const [a, b] = baseSeconds[t] ?? [10, 2];
  return (a > 0 ? (groupPhases[t] ?? 1) * d : 0) + b + 1;
}

/** One group: each test on its own (with a 1 s pause after each), then all together. */
export function groupSeconds(members: string[], d: number, baseline: boolean): number {
  const each = members.map((t) => groupedTestSeconds(t, d));
  return (baseline ? each.reduce((a, x) => a + x + 1, 0) : 0) + Math.max(0, ...each);
}

/**
 * Rough time for a list of planned steps: steps of a group's "together"
 * phase overlap, so the phase takes as long as its longest test.
 */
export function estimateSteps(steps: Pick<PlannedStep, 'test' | 'group' | 'phase'>[], scale: number, o: EstimateOpts = {}): number {
  const s = scale > 0 ? scale : 1;
  const d = o.groupDurationS && o.groupDurationS > 0 ? o.groupDurationS : groupDefaultDuration(s);
  let total = 3;
  const together = new Map<string, number>();
  for (const st of steps) {
    if (!st.group) total += testSeconds(st.test, s, o);
    else if (st.phase === 'alone') total += groupedTestSeconds(st.test, d) + 1;
    else together.set(st.group, Math.max(together.get(st.group) ?? 0, groupedTestSeconds(st.test, d)));
  }
  for (const v of together.values()) total += v;
  return total;
}

/** Rough run time in seconds of a test list ("a+b" entries are groups). */
export function estimateSeconds(tests: string[], scale: number, o: EstimateOpts = {}): number {
  return estimateSteps(planSteps(tests, o.baseline ?? true), scale, o);
}

/** The test list a run executes (custom list, else the profile's). */
export function runTests(run: Pick<Run, 'options' | 'profile'>, profiles?: Record<string, string[]>): string[] {
  const o = run.options;
  if (o?.tests && o.tests.length) return o.tests;
  const p = o?.profile || run.profile || 'standard';
  return profiles?.[p] ?? [];
}

export function runLabel(r: Pick<Run, 'agent_name' | 'agent_id' | 'options'>): string {
  if (r.agent_name) return r.agent_name;
  if (r.options?.imported) return `${r.options.client || 'imported'} (imported)`;
  return r.agent_id;
}

export const metricInfo: Record<string, { label: string; unit: string; desc: string }> = {
  rtt_p95: { label: 'RTT p95', unit: 'ms', desc: 'monitoring RTT p95' },
  loss_pct: { label: 'Loss', unit: '%', desc: 'monitoring loss' },
  jitter: { label: 'Jitter', unit: 'ms', desc: 'monitoring jitter' },
  offline: { label: 'Offline', unit: 'min', desc: 'minutes since the agent was last seen' },
  health_score: { label: 'Health score', unit: 'points', desc: 'health score of the latest run' },
  down_mbps: { label: 'Download', unit: 'Mbit/s', desc: 'download of the latest run' },
  up_mbps: { label: 'Upload', unit: 'Mbit/s', desc: 'upload of the latest run' },
};
