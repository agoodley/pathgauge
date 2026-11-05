import { useEffect, useMemo, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { estimateSeconds, runTests } from '../lib/domain';
import { between, fmtDuration, fmtMbps, fmtMs, fmtTime, f1, rateScale } from '../lib/format';
import { useTick } from '../lib/hooks';
import { live, useRunEvents } from '../lib/live';
import { Link } from '../lib/router';
import { usePalette } from '../lib/theme';
import type { CapacityStep, EngineEvent, Run, SweepStep } from '../lib/types';
import { Badge, Card, ConfirmButton, Empty, Notice, PageHeader, toast } from '../components/ui';
import { Figure, lineOption, type LineSeries } from '../components/Chart';
import { profileLabel, useProfiles } from '../components/shared';
import { SweepTable, bloatSeries, stepHeadline } from '../components/StepResult';
import { CapacityStepsTable, capacityMetric, capacityOption, capacityOverlap, OVERLAP_NOTE, SEARCH_LABEL } from '../components/MixResult';

interface StepState {
  index: number;
  test: string;
  title: string;
  stepId?: string;
  status: 'pending' | 'running' | 'ok' | 'error' | 'skipped' | 'earlier';
  seconds?: number;
  error?: string;
  headline?: string;
  startedAt?: number;
}

interface LiveState {
  tests: string[] | null;
  steps: Map<number, StepState>;
  current: StepState | null;
  sawStart: boolean;
  throughput: Map<string, [number, number][]>;
  latency: Map<string, [number, number][]>;
  sweep: SweepStep[];
  sqm: string[];
  throughputStep: string;
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
    current: null,
    sawStart: false,
    throughput: new Map(),
    latency: new Map(),
    sweep: [],
    sqm: [],
    throughputStep: '',
    latencyStep: '',
    sessionRtt: new Map(),
    sessionPhase: '',
    sessionTest: '',
    capacity: [],
  };
  // Per-direction time offsets so tests that restart their clock (stream
  // scaling, bufferbloat phases) draw left to right.
  const lastT = new Map<string, number>();
  const offset = new Map<string, number>();
  // Mix and capacity phases each restart their clock at 0: lay them end to end.
  const phaseOffset = new Map<string, number>();
  let phaseEnd = 0;
  for (const e of events) {
    switch (e.type) {
      case 'run_start':
        st.sawStart = true;
        if (Array.isArray(e.data)) st.tests = e.data.map(String);
        break;
      case 'step_start': {
        const s: StepState = { index: e.index ?? st.steps.size + 1, test: e.test ?? '', title: e.title ?? e.test ?? '', stepId: e.step, status: 'running' };
        st.steps.set(s.index, s);
        st.current = s;
        // New step: reset the live charts.
        st.throughput = new Map();
        st.latency = new Map();
        st.sweep = [];
        st.sqm = [];
        st.sessionRtt = new Map();
        st.sessionPhase = '';
        st.sessionTest = '';
        st.capacity = [];
        lastT.clear();
        offset.clear();
        phaseOffset.clear();
        phaseEnd = 0;
        break;
      }
      case 'step_done': {
        const idx = e.index ?? 0;
        const prev = st.steps.get(idx);
        const status = e.status === 'ok' ? 'ok' : e.status === 'skipped' ? 'skipped' : 'error';
        let headline = '';
        if (status === 'ok') {
          const d = Array.isArray(e.data) ? undefined : e.data;
          headline = stepHeadline(e.test ?? '', d);
        }
        st.steps.set(idx, {
          ...(prev ?? { index: idx, test: e.test ?? '', title: e.title ?? '' }),
          title: e.title || prev?.title || e.test || '',
          status,
          seconds: e.seconds,
          error: e.error,
          headline,
        });
        if (st.current?.index === idx) st.current = null;
        break;
      }
      case 'progress': {
        const d = (e.data ?? {}) as Record<string, unknown>;
        const kind = d.kind;
        if (kind === 'throughput') {
          const dir = String(d.direction ?? 'down');
          let t = Number(d.t);
          const prevT = lastT.get(dir);
          if (prevT !== undefined && t + (offset.get(dir) ?? 0) < prevT - 0.01) offset.set(dir, prevT);
          t += offset.get(dir) ?? 0;
          lastT.set(dir, t);
          const arr = st.throughput.get(dir) ?? [];
          arr.push([Math.round(t * 100) / 100, Number(d.mbps)]);
          st.throughput.set(dir, arr);
          st.throughputStep = st.current?.title ?? '';
        } else if (kind === 'latency' && isSessionTest(e.test ?? st.current?.test ?? '')) {
          const test = e.test ?? st.current?.test ?? '';
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
          st.latencyStep = st.current?.title ?? '';
        } else if (kind === 'capacity' && d.step && typeof d.step === 'object') {
          st.capacity.push({ ...(d.step as CapacityStep), search: String(d.search ?? 'alone') });
          st.sessionTest = e.test ?? st.current?.test ?? st.sessionTest;
        } else if (kind === 'latency') {
          const ph = String(d.phase ?? 'idle');
          const arr = st.latency.get(ph) ?? [];
          arr.push([Number(d.t), Number(d.rtt_ms)]);
          st.latency.set(ph, arr);
          st.latencyStep = st.current?.title ?? '';
        } else if (kind === 'sweep' && d.step && typeof d.step === 'object') {
          st.sweep.push(d.step as SweepStep);
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
  const total = tests.length;
  // Steps before the first one this page saw start finished before it
  // connected (the API has no history of live events).
  const firstSeen = Math.min(...[...st.steps.keys()], Infinity);
  const rows: StepState[] = tests.map(
    (t, i) =>
      st.steps.get(i + 1) ?? { index: i + 1, test: t, title: profiles?.titles[t] ?? t, status: i + 1 < firstSeen && firstSeen !== Infinity ? 'earlier' : 'pending' },
  );
  const done = rows.filter((r) => r.status === 'ok' || r.status === 'error' || r.status === 'skipped' || r.status === 'earlier').length;
  const elapsed = between(run.started_at || run.created_at, new Date().toISOString()) ?? 0;
  const remaining = estimateSeconds(
    rows.filter((r) => r.status === 'pending' || r.status === 'running').map((r) => r.test),
    run.options?.scale ?? 1,
  );

  const thrOpt = useMemo(() => {
    const series: LineSeries[] = [...st.throughput.entries()]
      .sort((a, b) => (DIR_SLOT[a[0]] ?? 3) - (DIR_SLOT[b[0]] ?? 3))
      .map(([dir, pts]) => ({ name: DIR_LABEL[dir] ?? dir, data: pts, slot: DIR_SLOT[dir] ?? 3 }));
    const max = Math.max(0, ...series.flatMap((s) => s.data.map((p) => p[1] ?? 0)));
    const sc = rateScale(max);
    return lineOption(P, {
      series: series.map((s) => ({ ...s, data: s.data.map(([x, y]) => [x, y === null ? null : y / sc.div] as [number, number | null]) })),
      xName: 'seconds',
      yName: sc.unit,
      yFmt: (y) => fmtMbps(y * sc.div),
      xFmt: (x) => `${f1(x)} s`,
    });
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

  const sweepOpt = useMemo(() => {
    const max = Math.max(0, ...st.sweep.map((s) => Math.max(s.offered_mbps, s.delivered_mbps)));
    const sc = rateScale(max);
    const off = st.sweep.map((s) => s.offered_mbps / sc.div);
    return lineOption(P, {
      series: [
        { name: 'delivered', data: st.sweep.map((s, i) => [off[i], s.delivered_mbps / sc.div]), points: false },
        { name: 'offered (ideal)', data: off.map((x) => [x, x]), slot: 2, dashed: true },
      ],
      xName: `offered (${sc.unit})`,
      yName: `delivered (${sc.unit})`,
      yFmt: (y) => fmtMbps(y * sc.div),
      xFmt: (x) => `offered ${fmtMbps(x * sc.div)}`,
    });
  }, [st, P]);

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
        <div className="progress" role="progressbar" aria-valuemin={0} aria-valuemax={total} aria-valuenow={done} aria-valuetext={`${done} of ${total} tests done`}>
          <div className="progress-fill" style={{ width: `${pct}%` }} />
        </div>
        <div className="progress-text">
          {done} of {total} tests done{total > done ? ` · about ${fmtDuration(remaining)} left` : ' · finishing…'}
        </div>
      </div>
      {!st.sawStart && events.length === 0 && (
        <Notice kind="info">Waiting for the agent… live progress appears as each step starts. (Steps that finished before this page opened are shown when the run completes.)</Notice>
      )}
      <div className="live-grid">
        <Card title="Steps" className="live-steps">
          <ol className="steps">
            {rows.map((r) => (
              <li key={r.index} className={`step-row step-${r.status}`}>
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
            ))}
          </ol>
        </Card>
        <div className="live-charts">
          {(st.throughput.size > 0 || (st.sweep.length === 0 && st.sqm.length === 0 && st.sessionRtt.size === 0 && st.capacity.length === 0)) && (
            <Card title={`Live throughput${st.throughputStep ? ` · ${st.throughputStep}` : ''}`}>
              {st.throughput.size ? (
                <Figure title="Throughput at the receiver" option={thrOpt} height={240} />
              ) : (
                <Empty>Throughput appears while a TCP, stream-scaling or bufferbloat test runs.</Empty>
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
            <Card title={`Capacity search · ${st.current?.title ?? st.latencyStep}`}>
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
          {st.sweep.length > 0 && (
            <Card title={`Live rate sweep · ${st.current?.title ?? ''}`}>
              <Figure title="Delivered vs offered rate" option={sweepOpt} height={220} />
              <SweepTable steps={st.sweep} />
            </Card>
          )}
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
