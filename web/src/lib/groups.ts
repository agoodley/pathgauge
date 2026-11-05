// Test groups: a test-list entry joining tests with "+" ("voip+cdp-up")
// runs them at the same time, each for the group duration, by default
// after running each on its own for comparison. Mirrors
// internal/engine/groups.go (checkGroup, groupDuration, the step IDs of
// runGroup/runStep) and phaseLabel/loadSection in internal/report/mix.go;
// keep them in step.
import type { Report, Step } from './types';

/** Tests that can't share the path with others, and why (notInGroups). */
export const NOT_IN_GROUPS: Record<string, string> = {
  sqm: 'it changes the interface’s shaping',
  'capacity-voice': 'it runs its own sequence of loads',
  'capacity-video': 'it runs its own sequence of loads',
};

export const isGroup = (entry: string) => entry.includes('+');
export const groupMembers = (entry: string) => entry.split('+');

/** Default time each grouped test runs: 30 s × scale, at least 15 s. */
export function groupDefaultDuration(scale: number): number {
  return Math.max(15, 30 * (scale > 0 ? scale : 1));
}

/** Why a group can't run, or null (same rules as checkGroup). */
export function groupError(members: string[]): string | null {
  if (members.length < 2) return 'add at least two tests';
  const bad = members.find((t) => NOT_IN_GROUPS[t]);
  if (bad) return `${bad} can’t run in a group: ${NOT_IN_GROUPS[bad]}`;
  if (new Set(members).size !== members.length) return 'a test appears twice';
  return null;
}

/** A step the run will produce, in run order. */
export interface PlannedStep {
  /** Step ID the engine gives it (s03-g1-alone-voip). */
  id: string;
  /** Position of its entry in the test list, from 1 (shared by a group's steps). */
  index: number;
  test: string;
  group?: string;
  phase?: 'alone' | 'together';
  /** The group's other tests. */
  others?: string[];
}

const pad = (n: number) => String(n).padStart(2, '0');

/**
 * The steps a test list produces: one per test, and for each group its
 * tests on their own (unless baseline is false), then together.
 */
export function planSteps(tests: string[], baseline = true): PlannedStep[] {
  const out: PlannedStep[] = [];
  let groups = 0;
  tests.forEach((entry, i) => {
    const index = i + 1;
    if (!isGroup(entry)) {
      out.push({ id: `s${pad(index)}-${entry}`, index, test: entry });
      return;
    }
    const gid = `g${++groups}`;
    const members = groupMembers(entry);
    const phases: ('alone' | 'together')[] = baseline ? ['alone', 'together'] : ['together'];
    for (const phase of phases)
      for (const t of members)
        out.push({ id: `s${pad(index)}-${gid}-${phase}-${t}`, index, test: t, group: gid, phase, others: members.filter((m) => m !== t) });
  });
  return out;
}

/** Tests in the group other than the step's own (othersThan). */
export function othersThan(tests: string[], stepId: string): string[] {
  return tests.filter((t) => !stepId.endsWith(`-${t}`) && !stepId.endsWith(`-${t}-fwd`) && !stepId.endsWith(`-${t}-rev`));
}

/** "on its own, for comparison" or "together with voip, cdp-up" (phaseLabel), or "". */
export function phaseText(rep: Pick<Report, 'groups'> | null | undefined, s: Pick<Step, 'id' | 'group' | 'phase'>): string {
  if (s.phase === 'alone') return 'on its own, for comparison';
  if (s.phase === 'together') {
    const g = (rep?.groups ?? []).find((x) => x.id === s.group);
    const others = g ? othersThan(g.tests, s.id) : [];
    return others.length ? `together with ${others.join(', ')}` : 'together with the group';
  }
  return '';
}

/** A compared value with its unit (loadVal). */
export function loadVal(v: number, unit: string): string {
  if (!Number.isFinite(v)) return '—';
  if (unit === '') return v.toFixed(2);
  if (unit === '%') return `${v.toFixed(1)}%`;
  return `${v.toFixed(1)} ${unit}`;
}
