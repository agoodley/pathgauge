// Domain helpers: status classes, severities, duration estimates.
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
};

export function estimateSeconds(tests: string[], scale: number): number {
  const s = scale > 0 ? scale : 1;
  let total = 3;
  for (const t of tests) {
    const [a, b] = baseSeconds[t] ?? [10, 2];
    total += a * s + b + 1;
  }
  return total;
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
