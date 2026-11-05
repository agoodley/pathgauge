// Monitoring charts (RTT p50/p95, jitter, loss: one measure family per
// chart, never a dual axis) and the hour × weekday heatmap.
import { useMemo, useRef } from 'react';
import { useApi } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { usePalette } from '../lib/theme';
import { fmtMs, fmtPct, isNum } from '../lib/format';
import type { Annotation, HeatCell, Sample } from '../lib/types';
import { Figure, heatmapOption, lineOption, type Band, type LineSeries } from './Chart';
import { DataTable, Empty, ErrorBox, Loading, Tile } from './ui';

export const RANGES = [
  { id: '6h', label: '6 h', hours: 6 },
  { id: '24h', label: '24 h', hours: 24 },
  { id: '7d', label: '7 days', hours: 24 * 7 },
  { id: '30d', label: '30 days', hours: 24 * 30 },
] as const;

export type RangeId = (typeof RANGES)[number]['id'];

const timeFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });

function withGaps(samples: Sample[], step: number, pick: (s: Sample) => number): [number, number | null][] {
  const out: [number, number | null][] = [];
  let prev: number | null = null;
  for (const s of samples) {
    if (prev !== null && s.ts - prev > step * 2.5) out.push([(prev + step) * 1000, null]);
    out.push([s.ts * 1000, isNum(pick(s)) ? Math.round(pick(s) * 1000) / 1000 : null]);
    prev = s.ts;
  }
  return out;
}

export function MonitorCharts({
  agentId,
  hours,
  annotations,
  showTiles = true,
}: {
  agentId: string;
  hours: number;
  annotations?: Annotation[];
  showTiles?: boolean;
}) {
  const P = usePalette();
  const mon = useApi<{ step: number; samples: Sample[] }>(`/api/monitor/${encodeURIComponent(agentId)}?hours=${hours}`);
  const last = useRef(0);
  useLiveRefresh(
    (m) => {
      if (m.type !== 'monitor' || m.agent_id !== agentId) return false;
      // Refresh at most every 30 s; buckets are minutes wide anyway.
      if (Date.now() - last.current < 30000) return false;
      last.current = Date.now();
      return true;
    },
    mon.reload,
    300,
  );

  const now = Date.now();
  const from = now - hours * 3600 * 1000;

  const charts = useMemo(() => {
    const d = mon.data;
    if (!d) return null;
    const ss = d.samples;
    const step = d.step || 60;
    const bands: Band[] = (annotations ?? [])
      .filter((a) => a.ts_end * 1000 >= from && a.ts_start * 1000 <= now)
      .map((a) => ({
        from: Math.max(a.ts_start * 1000, from),
        to: Math.max(a.ts_end, a.ts_start + step) * 1000,
        label: a.text.length > 24 ? a.text.slice(0, 23) + '…' : a.text,
      }));
    const xFmt = (v: number) => timeFmt.format(new Date(v));
    const common = { xName: 'time', xType: 'time' as const, xMin: from, xMax: now, xFmt, bands };
    const rtt: LineSeries[] = [
      { name: 'p50', data: withGaps(ss, step, (s) => s.rtt_p50), slot: 0 },
      { name: 'p95', data: withGaps(ss, step, (s) => s.rtt_p95), slot: 1 },
    ];
    return {
      rtt: lineOption(P, { ...common, series: rtt, yName: 'RTT (ms)', yFmt: (v) => fmtMs(v) }),
      jitter: lineOption(P, { ...common, series: [{ name: 'jitter', data: withGaps(ss, step, (s) => s.jitter) }], yName: 'jitter (ms)', yFmt: (v) => fmtMs(v) }),
      loss: lineOption(P, {
        ...common,
        series: [{ name: 'loss', data: withGaps(ss, step, (s) => s.loss_pct) }],
        yName: 'loss (%)',
        yFmt: (v) => fmtPct(v),
      }),
      step,
    };
    // `now` changes every render; recompute only when data/theme/annotations change.
  }, [mon.data, P, annotations, hours]);

  if (mon.error) return <ErrorBox error={mon.error} onRetry={mon.reload} />;
  if (!mon.data || !charts) return <Loading />;
  const ss = mon.data.samples;
  if (!ss.length)
    return <Empty>No monitoring samples in this period. Enable monitoring in the agent's settings to collect them.</Empty>;

  const lastS = ss[ss.length - 1];
  const avg = (f: (s: Sample) => number) => ss.reduce((a, s) => a + f(s), 0) / ss.length;
  const stepLabel = charts.step >= 3600 ? `${charts.step / 3600} h` : `${charts.step / 60} min`;
  return (
    <div>
      {showTiles && (
        <div className="tiles">
          <Tile label="RTT p50 (latest)" value={fmtMs(lastS.rtt_p50)} sub={`period average ${fmtMs(avg((s) => s.rtt_p50))}`} />
          <Tile label="RTT p95 (latest)" value={fmtMs(lastS.rtt_p95)} sub={`period worst ${fmtMs(Math.max(...ss.map((s) => s.rtt_p95)))}`} />
          <Tile label="Jitter (latest)" value={fmtMs(lastS.jitter)} sub={`period average ${fmtMs(avg((s) => s.jitter))}`} />
          <Tile label="Loss (latest)" value={fmtPct(lastS.loss_pct)} sub={`period average ${fmtPct(avg((s) => s.loss_pct))}`} />
        </div>
      )}
      <p className="muted small">
        {ss.length} points, {stepLabel} buckets (RTT p50 averaged, p95 worst per bucket). Shaded areas are annotations.
      </p>
      <div className="chart-stack">
        <Figure title="Round-trip time" option={charts.rtt} height={240} />
        <div className="grid-2 tight">
          <Figure title="Jitter" option={charts.jitter} height={200} />
          <Figure title="Packet loss" option={charts.loss} height={200} />
        </div>
      </div>
      <details className="table-toggle">
        <summary>Show as a table ({ss.length} rows)</summary>
        <DataTable
          head={['Time', 'RTT min', 'RTT p50', 'RTT p95', 'RTT max', 'Jitter', 'Loss', 'Probes']}
          numeric={[1, 2, 3, 4, 5, 6, 7]}
          rows={[...ss].reverse().map((s) => [
            timeFmt.format(new Date(s.ts * 1000)),
            fmtMs(s.rtt_min),
            fmtMs(s.rtt_p50),
            fmtMs(s.rtt_p95),
            fmtMs(s.rtt_max),
            fmtMs(s.jitter),
            fmtPct(s.loss_pct),
            s.sent,
          ])}
        />
      </details>
    </div>
  );
}

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
// Show Monday first.
const ROW_ORDER = [1, 2, 3, 4, 5, 6, 0];

export function Heatmap({ agentId, days = 28 }: { agentId: string; days?: number }) {
  const P = usePalette();
  const hm = useApi<HeatCell[] | null>(`/api/monitor/${encodeURIComponent(agentId)}/heatmap?days=${days}`);
  const opt = useMemo(() => {
    const cells = hm.data ?? [];
    const hours = Array.from({ length: 24 }, (_, h) => String(h).padStart(2, '0'));
    const rows = ROW_ORDER.map((d) => DAYS[d]);
    const data: [number, number, number, string][] = cells.map((c) => [
      c.hour,
      ROW_ORDER.indexOf(c.weekday),
      Math.round(c.rtt_p95 * 100) / 100,
      `RTT p95 ${fmtMs(c.rtt_p95)} · loss ${fmtPct(c.loss_pct)} · ${c.n} samples`,
    ]);
    return { opt: heatmapOption(P, { xCats: hours, yCats: rows, data, xName: 'hour of day (UTC)', yName: '', unit: 'ms' }), cells };
  }, [hm.data, P]);
  if (hm.error) return <ErrorBox error={hm.error} onRetry={hm.reload} />;
  if (hm.loading && !hm.data) return <Loading />;
  if (!opt.cells.length) return <Empty>Not enough monitoring data for a heatmap yet.</Empty>;
  return (
    <>
      <Figure title={`Average RTT p95 by hour and weekday (UTC), last ${days} days`} option={opt.opt} height={300} />
      <details className="table-toggle">
        <summary>Show as a table</summary>
        <DataTable
          head={['Weekday', 'Hour (UTC)', 'RTT p95', 'Loss', 'Samples']}
          numeric={[2, 3, 4]}
          rows={[...opt.cells]
            .sort((a, b) => ROW_ORDER.indexOf(a.weekday) - ROW_ORDER.indexOf(b.weekday) || a.hour - b.hour)
            .map((c) => [DAYS[c.weekday], `${String(c.hour).padStart(2, '0')}:00`, fmtMs(c.rtt_p95), fmtPct(c.loss_pct), c.n])}
        />
      </details>
    </>
  );
}
