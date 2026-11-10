import { Fragment, useEffect, useMemo, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { estimateSteps, runTests } from '../lib/domain';
import { groupDefaultDuration, planSteps } from '../lib/groups';
import { between, fmtDuration, fmtMbps, fmtMs, fmtTime, f0, f1, rateScale } from '../lib/format';
import { useTick } from '../lib/hooks';
import { live, useRunEvents } from '../lib/live';
import { Link } from '../lib/router';
import { usePalette, type Palette } from '../lib/theme';
import type { CapacityStep, EngineEvent, Run, SweepStep } from '../lib/types';
import { Badge, Card, ConfirmButton, Empty, Notice, PageHeader, toast } from '../components/ui';
import { Figure, lineOption, type LineSeries } from '../components/Chart';
import { profileLabel, useProfiles } from '../components/shared';
import { SweepTable, bloatSeries, stepHeadline } from '../components/StepResult';
import { CapacityStepsTable, capacityMetric, capacityOption, capacityOverlap, OVERLAP_NOTE, SEARCH_LABEL } from '../components/MixResult';

interface StepState {
  /** Step ID (s03-tcp-down, s04-g1-together-voip). */
  id: string;
  index: number;
  test: string;
  title: string;
  group?: string;
  phase?: 'alone' | 'together';
  status: 'pending' | 'running' | 'ok' | 'error' | 'skipped' | 'earlier';
  seconds?: number;
  error?: string;
  headline?: string;
}

/** Live throughput of one step in one direction. */
interface ThroughputSeries {
  step: string;
  test: string;
  dir: string;
  points: [number, number][];
}

interface LiveState {
  tests: string[] | null;
  /**
   * Steps seen so far, by step ID. While a group's tests run together
   * several are running at once, and they share their entry's index.
   */
  steps: Map<string, StepState>;
  sawStart: boolean;
  /** By step and direction: a group's steps interleave their samples. */
  throughput: Map<string, ThroughputSeries>;
  latency: Map<string, [number, number][]>;
  /** Rate sweeps by step. */
  sweep: Map<string, SweepStep[]>;
  sqm: string[];
  latencyStep: string;
  /** Traffic mix / call capacity: probe RTT by series, phases end to end. */
  sessionRtt: Map<string, [number, number | null][]>;
  /** The phase the latest probe sample came from ("with-transfers", "8 sessions", …). */
  sessionPhase: string;
  sessionTest: string;
  /** Call capacity steps in the order they finished. */
  capacity: (CapacityStep & { search: string })[];
}

const isSessionTest = (t: string) => t === 'mix' || t.startsWith('capacity-');

function derive(events: EngineEvent[]): LiveState {
  const st: LiveState = {
    tests: null,
    steps: new Map(),
    sawStart: false,
    throughput: new Map(),
    latency: new Map(),
    sweep: new Map(),
    sqm: [],
    latencyStep: '',
    sessionRtt: new Map(),
    sessionPhase: '',
    sessionTest: '',
    capacity: [],
  };
  // Per-series time offsets so tests that restart their clock (stream
  // scaling, bufferbloat phases) draw left to right.
  const lastT = new Map<string, number>();
  const offset = new Map<string, number>();
  // Mix and capacity phases each restart their clock at 0: lay them end to end.
  const phaseOffset = new Map<string, number>();
  let phaseEnd = 0;
  const running = new Set<string>();
  const keyOf = (e: EngineEvent) => e.step || `#${e.index ?? 0}`;
  const titleOf = (e: EngineEvent) => st.steps.get(keyOf(e))?.title ?? e.title ?? e.test ?? '';
  for (const e of events) {
    switch (e.type) {
      case 'run_start':
        st.sawStart = true;
        if (Array.isArray(e.data)) st.tests = e.data.map(String);
        break;
      case 'step_start': {
        const id = keyOf(e);
        // A new step resets the live charts, unless other steps are still
        // running (a group's tests start together).
        if (running.size === 0) {
          st.throughput = new Map();
          st.latency = new Map();
          st.sweep = new Map();
          st.sqm = [];
          st.sessionRtt = new Map();
          st.sessionPhase = '';
          st.sessionTest = '';
          st.capacity = [];
          lastT.clear();
          offset.clear();
          phaseOffset.clear();
          phaseEnd = 0;
        }
        running.add(id);
        st.steps.set(id, { id, index: e.index ?? 0, test: e.test ?? '', title: e.title ?? e.test ?? '', status: 'running' });
        break;
      }
      case 'step_done': {
        const id = keyOf(e);
        running.delete(id);
        const prev = st.steps.get(id);
        const status = e.status === 'ok' ? 'ok' : e.status === 'skipped' ? 'skipped' : 'error';
        let headline = '';
        if (status === 'ok') {
          const d = Array.isArray(e.data) ? undefined : e.data;
          headline = stepHeadline(e.test ?? '', d);
        }
        st.steps.set(id, {
          ...(prev ?? { id, index: e.index ?? 0, test: e.test ?? '', title: e.title ?? '' }),
          title: e.title || prev?.title || e.test || '',
          status,
          seconds: e.seconds,
          error: e.error,
          headline,
        });
        break;
      }
      case 'progress': {
        const d = (e.data ?? {}) as Record<string, unknown>;
        const kind = d.kind;
        const id = keyOf(e);
        const test = e.test ?? st.steps.get(id)?.test ?? '';
        if (kind === 'throughput') {
          const dir = String(d.direction ?? 'down');
          const key = `${id}|${dir}`;
          let t = Number(d.t);
          const prevT = lastT.get(key);
          if (prevT !== undefined && t + (offset.get(key) ?? 0) < prevT - 0.01) offset.set(key, prevT);
          t += offset.get(key) ?? 0;
          lastT.set(key, t);
          const s = st.throughput.get(key) ?? { step: id, test, dir, points: [] };
          s.points.push([Math.round(t * 100) / 100, Number(d.mbps)]);
          st.throughput.set(key, s);
        } else if (kind === 'latency' && isSessionTest(test)) {
          const ph = String(d.phase ?? '');
          const rtt = Number(d.rtt_ms);
          if (!Number.isFinite(rtt)) break;
          // Mix: one series per phase. Capacity: one per search, so the
          // steps ("8 sessions", "8 sessions with transfers") join up.
          const key = test === 'mix' ? ph : / with transfers$/.test(ph) ? 'with-transfers' : 'alone';
          const arr = st.sessionRtt.get(key) ?? [];
          if (!phaseOffset.has(ph)) {
            const start = phaseEnd > 0 ? phaseEnd + 0.5 : 0;
            phaseOffset.set(ph, start);
            // Break the line across the pause between two steps.
            if (arr.length) arr.push([start - 0.25, null]);
          }
          const t = Math.round((Number(d.t) + (phaseOffset.get(ph) ?? 0)) * 100) / 100;
          phaseEnd = Math.max(phaseEnd, t);
          arr.push([t, rtt]);
          st.sessionRtt.set(key, arr);
          st.sessionPhase = ph;
          st.sessionTest = test;
          st.latencyStep = titleOf(e);
        } else if (kind === 'capacity' && d.step && typeof d.step === 'object') {
          st.capacity.push({ ...(d.step as CapacityStep), search: String(d.search ?? 'alone') });
          st.sessionTest = test || st.sessionTest;
        } else if (kind === 'latency') {
          const ph = String(d.phase ?? 'idle');
          const arr = st.latency.get(ph) ?? [];
          arr.push([Number(d.t), Number(d.rtt_ms)]);
          st.latency.set(ph, arr);
          st.latencyStep = titleOf(e);
        } else if (kind === 'sweep' && d.step && typeof d.step === 'object') {
          const arr = st.sweep.get(id) ?? [];
          arr.push(d.step as SweepStep);
          st.sweep.set(id, arr);
        } else if (kind === 'sqm' && typeof d.message === 'string') {
          st.sqm.push(d.message);
        }
        break;
      }
    }
  }
  return st;
}

const DIR_LABEL: Record<string, string> = { down: 'download', up: 'upload', bidir: 'both' };
// Same names as the result page (MixPhase.label, capacity searches).
const SESSION_SERIES: Record<string, [label: string, slot: number]> = {
  interactive: ['without transfers', 0],
  'with-transfers': ['with file transfers', 1],
  all: ['all sessions', 0],
  alone: ['on their own', 0],
};
const DIR_SLOT: Record<string, number> = { down: 0, up: 1, bidir: 2 };

function sweepOption(P: Palette, list: SweepStep[]) {
  const max = Math.max(0, ...list.map((s) => Math.max(s.offered_mbps, s.delivered_mbps)));
  const sc = rateScale(max);
  const off = list.map((s) => s.offered_mbps / sc.div);
  return lineOption(P, {
    series: [
      { name: 'delivered', data: list.map((s, i) => [off[i], s.delivered_mbps / sc.div]), points: false },
      { name: 'offered (ideal)', data: off.map((x) => [x, x]), slot: 2, dashed: true },
    ],
    xName: `offered (${sc.unit})`,
    yName: `delivered (${sc.unit})`,
    yFmt: (y) => fmtMbps(y * sc.div),
    xFmt: (x) => `offered ${fmtMbps(x * sc.div)}`,
  });
}

export function LiveRun({ run, onChange }: { run: Run; onChange: () => void }) {
  const P = usePalette();
  const profiles = useProfiles();
  const canCancel = useCan('operator');
  const [events, version] = useRunEvents(run.id);
  useTick(1000);
  // Load the events of this run that happened before the page opened.
  useEffect(() => {
    if (run.status !== 'running') return;
    api
      .get<{ seq: number; event: EngineEvent }[]>(`/api/runs/${run.id}/events`)
      .then((h) => live.seed(run.id, h ?? []))
      .catch(() => {});
  }, [run.id, run.status]);
  const [cancelled, setCancelled] = useState(false);

  const st = useMemo(() => derive(events), [events, version]);
  const tests = st.tests ?? runTests(run, profiles?.profiles);
  const baseline = !run.options?.no_baseline;
  const testsKey = tests.join(',');
  const plan = useMemo(() => planSteps(testsKey ? testsKey.split(',') : [], baseline), [testsKey, baseline]);
  const hasGroups = plan.some((p) => p.group);
  // One row per planned step (a group's tests on their own, then together),
  // plus any step the plan didn't foresee. Rows before the first one this
  // page saw finished before it connected.
  const rows: StepState[] = useMemo(() => {
    const firstSeen = plan.findIndex((p) => st.steps.has(p.id));
    const out: StepState[] = plan.map((p, i) => {
      const s = st.steps.get(p.id);
      if (s) return { ...s, group: p.group, phase: p.phase };
      return {
        id: p.id,
        index: p.index,
        test: p.test,
        title: profiles?.titles[p.test] ?? p.test,
        group: p.group,
        phase: p.phase,
        status: firstSeen > i ? 'earlier' : 'pending',
      };
    });
    for (const s of st.steps.values()) if (!plan.some((p) => p.id === s.id)) out.push(s);
    return out;
  }, [plan, st, profiles]);
  const total = rows.length;
  const done = rows.filter((r) => r.status === 'ok' || r.status === 'error' || r.status === 'skipped' || r.status === 'earlier').length;
  const elapsed = between(run.started_at || run.created_at, new Date().toISOString()) ?? 0;
  const groupDur = run.options?.group_duration_s || groupDefaultDuration(run.options?.scale ?? 1);
  const remaining = estimateSteps(
    rows.filter((r) => r.status === 'pending' || r.status === 'running'),
    run.options?.scale ?? 1,
    { cdpDurationS: run.options?.cdp?.duration_s, groupDurationS: run.options?.group_duration_s, baseline },
  );
  const stepTitle = (id: string) => rows.find((r) => r.id === id)?.title ?? '';

  // Throughput: one step as before (series by direction); several steps
  // running together get a series each, named by test, colours in the
  // order they appeared.
  const thr = useMemo(() => {
    const list = [...st.throughput.values()];
    const steps = [...new Set(list.map((s) => s.step))];
    const many = steps.length > 1;
    const series: LineSeries[] = many
      ? list.map((s, i) => ({ name: `${DIR_LABEL[s.dir] ?? s.dir} · ${s.test}`, data: s.points, ...(i < 4 ? { slot: i } : { color: P.muted }) }))
      : list
          .map((s) => ({ name: DIR_LABEL[s.dir] ?? s.dir, data: s.points as [number, number | null][], slot: DIR_SLOT[s.dir] ?? 3 }))
          .sort((a, b) => a.slot - b.slot);
    const max = Math.max(0, ...series.flatMap((s) => s.data.map((p) => p[1] ?? 0)));
    const sc = rateScale(max);
    const option = lineOption(P, {
      series: series.map((s) => ({ ...s, data: s.data.map(([x, y]) => [x, y === null ? null : y / sc.div] as [number, number | null]) })),
      // Samples carry each step's own clock (no shared timestamp).
      xName: many ? 'seconds since each test started' : 'seconds',
      yName: sc.unit,
      yFmt: (y) => fmtMbps(y * sc.div),
      xFmt: (x) => `${f1(x)} s`,
    });
    return { option, steps, test: list[0]?.test ?? '' };
  }, [st, P]);

  const latOpt = useMemo(
    () =>
      lineOption(P, {
        series: bloatSeries(
          [...st.latency.entries()].map(([name, points]) => ({ name, points })),
          P.muted,
        ),
        xName: 'seconds',
        yName: 'RTT (ms)',
        yFmt: (y) => fmtMs(y),
        xFmt: (x) => `${f1(x)} s`,
      }),
    [st, P],
  );

  const capKind = st.sessionTest.replace(/^capacity-/, '');
  const sessOpt = useMemo(() => {
    const cap = st.sessionTest.startsWith('capacity-');
    const series: LineSeries[] = [...st.sessionRtt.entries()]
      .map(([key, data]) => {
        const [name, slot] =
          cap && key === 'with-transfers' ? [SEARCH_LABEL['with-transfers'].toLowerCase(), 1] : (SESSION_SERIES[key] ?? [key, 2]);
        return { name, slot, data };
      })
      .sort((a, b) => a.slot - b.slot);
    return lineOption(P, {
      series,
      xName: 'seconds',
      yName: 'RTT (ms)',
      yFmt: (y) => fmtMs(y),
      xFmt: (x) => `${f1(x)} s`,
    });
  }, [st, P]);

  const capSearches = useMemo(
    () => ['alone', 'with-transfers'].map((search) => ({ search, steps: st.capacity.filter((c) => c.search === search) })),
    [st],
  );
  const capOpt = useMemo(() => capacityOption(P, capKind, capSearches), [P, capKind, capSearches]);

  const sweeps = useMemo(() => [...st.sweep.entries()].map(([id, list]) => ({ id, list, option: sweepOption(P, list) })), [st, P]);

  const cancel = async () => {
    try {
      await api.post(`/api/runs/${encodeURIComponent(run.id)}/cancel`);
      setCancelled(true);
      toast('Run cancelled');
      onChange();
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };

  const thrTitle = thr.steps.length > 1 ? `${thr.steps.length} tests together` : stepTitle(thr.steps[0] ?? '');
  const thrCDP = thr.steps.length === 1 && thr.test.startsWith('cdp-');
  const running = rows.filter((r) => r.status === 'running');
  const pct = total ? Math.round((100 * done) / total) : 0;
  return (
    <div className="page">
      <PageHeader
        title={
          <>
            Running: {run.agent_name || run.agent_id} <span className="muted">· {profileLabel(run)}</span>
          </>
        }
        sub={
          <span className="inline-list">
            <Badge kind="info" icon="◌" label="Running" />
            <span>started {fmtTime(run.started_at || run.created_at)}</span>
            <span>elapsed {fmtDuration(elapsed)}</span>
            {run.options?.scale && run.options.scale !== 1 && <span>scale {run.options.scale}</span>}
            <span>
              by {run.requested_by || run.trigger} ({run.trigger})
            </span>
          </span>
        }
        actions={
          canCancel && !cancelled ? <ConfirmButton label="Cancel run" confirmLabel="Cancel the run" prompt="Stop this run?" onConfirm={cancel} small={false} /> : undefined
        }
      />
      <div className="progress-wrap" aria-label="Run progress">
        <div
          className="progress"
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={total}
          aria-valuenow={done}
          aria-valuetext={`${done} of ${total} ${hasGroups ? 'steps' : 'tests'} done`}
        >
          <div className="progress-fill" style={{ width: `${pct}%` }} />
        </div>
        <div className="progress-text">
          {done} of {total} {hasGroups ? 'steps' : 'tests'} done{total > done ? ` · about ${fmtDuration(remaining)} left` : ' · finishing…'}
          {running.length > 1 && ` · ${running.length} running together`}
        </div>
      </div>
      {run.agent_away && (
        <Notice kind="warning">
          The agent has lost its connection to the server. The test carries on running on the agent; live progress resumes when it
          reconnects, and the result is uploaded when the run finishes.
        </Notice>
      )}
      {!st.sawStart && events.length === 0 && !run.agent_away && (
        <Notice kind="info">Waiting for the agent… live progress appears as each step starts. (Steps that finished before this page opened are shown when the run completes.)</Notice>
      )}
      <div className="live-grid">
        <Card title="Steps" className="live-steps">
          <ol className="steps">
            {rows.map((r, i) => (
              <Fragment key={r.id}>
                {r.group && (rows[i - 1]?.group !== r.group || rows[i - 1]?.phase !== r.phase) && (
                  <li className="step-group-head">
                    Group {r.group.replace(/^g/, '')}:{' '}
                    {r.phase === 'alone' ? `each test on its own for ${f0(groupDur)} s, for comparison` : `all together for ${f0(groupDur)} s`}
                  </li>
                )}
                <li className={`step-row step-${r.status}${r.group ? ' step-grouped' : ''}`}>
                  <StepIcon s={r.status} />
                  <div className="step-main">
                    <div className="step-title">{r.title}</div>
                    <div className="cell-sub">
                      {r.status === 'running' && 'running…'}
                      {r.status === 'ok' && `${r.seconds ? `${f1(r.seconds)} s` : '<0.1 s'}${r.headline ? ' · ' + r.headline : ''}`}
                      {r.status === 'error' && <span className="text-critical">{r.error || 'failed'}</span>}
                      {r.status === 'skipped' && 'skipped'}
                      {r.status === 'earlier' && 'finished before this page opened'}
                      {r.status === 'pending' && <code className="small">{r.test}</code>}
                    </div>
                  </div>
                </li>
              </Fragment>
            ))}
          </ol>
        </Card>
        <div className="live-charts">
          {(st.throughput.size > 0 || (st.sweep.size === 0 && st.sqm.length === 0 && st.sessionRtt.size === 0 && st.capacity.length === 0)) && (
            <Card title={`Live throughput${thrTitle ? ` · ${thrTitle}` : ''}`}>
              {st.throughput.size ? (
                thrCDP ? (
                  <Figure
                    title="Replication throughput at the receiver (changed data sent)"
                    option={thr.option}
                    height={240}
                    note={`${thr.test === 'cdp-down' ? 'The server side is the source.' : 'This agent is the source.'} Write bursts show as peaks; a flat top that outlasts a burst means changes are waiting. The lag, the RPO the replica would have had, is worked out when the step ends.`}
                  />
                ) : thr.steps.length > 1 ? (
                  <Figure
                    title="Throughput at the receiver, by test"
                    option={thr.option}
                    height={240}
                    note="These tests run at the same time, so they share the path."
                  />
                ) : (
                  <Figure title="Throughput at the receiver" option={thr.option} height={240} />
                )
              ) : (
                <Empty>Throughput appears while a TCP, stream-scaling, bufferbloat or CDP replication test runs.</Empty>
              )}
            </Card>
          )}
          {st.latency.size > 0 && (
            <Card title={`Live latency under load · ${st.latencyStep}`}>
              <Figure title="Probe round-trip time by phase" option={latOpt} height={240} />
            </Card>
          )}
          {st.sessionRtt.size > 0 && (
            <Card title={`Live latency · ${st.latencyStep}`}>
              <Figure
                title={`Probe round-trip time while the sessions run${st.sessionPhase ? ` · now: ${phaseText(st.sessionPhase)}` : ''}`}
                option={sessOpt}
                height={240}
                note={st.sessionTest === 'mix' ? undefined : 'Each step adds sessions; gaps are the pauses between steps.'}
              />
            </Card>
          )}
          {st.capacity.length > 0 && (
            <Card title={`Capacity search · ${running.find((r) => r.test.startsWith('capacity-'))?.title ?? st.latencyStep}`}>
              {st.capacity.length >= 2 && (
                <Figure
                  title={`${capacityMetric(capKind).title} (so far)`}
                  option={capOpt}
                  height={220}
                  note={capacityOverlap(capSearches) ? OVERLAP_NOTE : undefined}
                />
              )}
              {capSearches
                .filter((x) => x.steps.length)
                .map((x) => (
                  <div key={x.search}>
                    <h4>{SEARCH_LABEL[x.search] ?? x.search}</h4>
                    <CapacityStepsTable kind={capKind} steps={x.steps} />
                  </div>
                ))}
            </Card>
          )}
          {sweeps.map((w) => (
            <Card key={w.id} title={`Live rate sweep · ${stepTitle(w.id)}`}>
              <Figure title="Delivered vs offered rate" option={w.option} height={220} />
              <SweepTable steps={w.list} />
            </Card>
          ))}
          {st.sqm.length > 0 && (
            <Card title="SQM tuner">
              <ul className="log">
                {st.sqm.map((m, i) => (
                  <li key={i}>{m}</li>
                ))}
              </ul>
            </Card>
          )}
        </div>
      </div>
      <p className="muted small">
        Run <code>{run.id}</code>. When the run finishes this page switches to the results. <Link to="/">Back to the overview</Link>
      </p>
    </div>
  );
}

/** A mix phase name or a capacity step label ("8 sessions with transfers") for display. */
function phaseText(ph: string): string {
  const s = SESSION_SERIES[ph];
  if (s) return s[0];
  return ph.replace(/^1 sessions/, '1 session');
}

function StepIcon({ s }: { s: StepState['status'] }) {
  switch (s) {
    case 'ok':
      return (
        <span className="step-icon ok" aria-label="Done" title="Done">
          ✓
        </span>
      );
    case 'error':
      return (
        <span className="step-icon err" aria-label="Failed" title="Failed">
          ✖
        </span>
      );
    case 'skipped':
      return (
        <span className="step-icon skip" aria-label="Skipped" title="Skipped">
          –
        </span>
      );
    case 'running':
      return <span className="spinner step-icon" role="img" aria-label="Running" title="Running" />;
    case 'earlier':
      return (
        <span className="step-icon skip" aria-label="Finished" title="Finished before this page opened">
          •
        </span>
      );
  }
  return (
    <span className="step-icon pending" aria-label="Pending" title="Pending">
      ○
    </span>
  );
}
