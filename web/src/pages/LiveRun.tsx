import { useEffect, useMemo, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { estimateSeconds, runTests } from '../lib/domain';
import { between, fmtDuration, fmtMbps, fmtMs, fmtTime, f1, rateScale } from '../lib/format';
import { useTick } from '../lib/hooks';
import { live, useRunEvents } from '../lib/live';
import { Link } from '../lib/router';
import { usePalette } from '../lib/theme';
import type { EngineEvent, Run, SweepStep } from '../lib/types';
import { Badge, Card, ConfirmButton, Empty, Notice, PageHeader, toast } from '../components/ui';
import { Figure, lineOption, type LineSeries } from '../components/Chart';
import { profileLabel, useProfiles } from '../components/shared';
import { SweepTable, bloatSeries, stepHeadline } from '../components/StepResult';

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
}

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
  };
  // Per-direction time offsets so tests that restart their clock (stream
  // scaling, bufferbloat phases) draw left to right.
  const lastT = new Map<string, number>();
  const offset = new Map<string, number>();
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
        lastT.clear();
        offset.clear();
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
          {(st.throughput.size > 0 || (st.sweep.length === 0 && st.sqm.length === 0)) && (
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
