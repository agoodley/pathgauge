// Renderer for the CDP replication tests (cdp-up, cdp-down), mirroring
// cdpSection in internal/report/mix.go so the dashboard and the HTML
// report agree.
import { useMemo } from 'react';
import { verdictInfo } from '../lib/domain';
import { f0, f1, f2, fmtMbps, fmtMs, isNum, rateScale } from '../lib/format';
import { usePalette } from '../lib/theme';
import type { CDPResult } from '../lib/types';
import { Figure, lineOption } from './Chart';
import { VerdictBadge } from './MixResult';
import { DataTable, Tile } from './ui';

/** Seconds as "3 s" or "2.5 s". */
const num = (v: number | null | undefined) => (isNum(v) ? String(+v.toFixed(1)) : '—');
const secs = (v: number | null | undefined) => (isNum(v) ? `${f1(v)} s` : '—');

/** A point's second, "3–4 s" (each point covers the second up to t). */
const second = (t: number) => (t > 0 ? `${f0(t - 1)}–${f0(t)} s` : '0 s');

/** Who was the source, in plain words. */
export function cdpSource(direction: string): string {
  return direction === 'down' ? 'The server side was the CDP source, replicating to this agent' : 'This agent was the CDP source, replicating to the server';
}

/** Share of time within the RPO without rounding 99.6% up to 100%. */
function withinText(p: number | null | undefined): string {
  if (!isNum(p)) return '—';
  if (p >= 99.95) return '100%';
  return p >= 99 ? `${f1(p)}%` : `${f0(p)}%`;
}

export function CDPView({ v }: { v: CDPResult }) {
  const P = usePalette();
  const set = v.settings ?? {};
  const rpo = set.rpo_s ?? 15;
  const pts = v.points ?? [];

  const charts = useMemo(() => {
    // Each point covers the second up to t; at 0 nothing is produced,
    // sent or waiting.
    const at0 = (d: [number, number][]): [number, number][] => (d.length && d[0][0] > 0 ? [[0, 0], ...d] : d);
    const lag = lineOption(P, {
      series: [{ name: 'lag', data: at0(pts.map((p) => [p.t, p.lag_s])) }],
      refLines: [{ y: rpo, label: `target RPO ${num(rpo)} s` }],
      xName: 'seconds',
      xMin: 0,
      yName: 'lag (s)',
      yFmt: (y) => `${f2(y)} s`,
      xFmt: second,
    });
    const max = Math.max(0, ...pts.map((p) => Math.max(p.gen_mbps, p.sent_mbps)));
    const sc = rateScale(max);
    const rate = lineOption(P, {
      series: [
        // Per-second totals: each value holds over its second, so bursts
        // keep their shape. Produced has a wash so it shows where sent
        // covers it.
        { name: 'produced', data: at0(pts.map((p) => [p.t, p.gen_mbps / sc.div])), step: 'start', area: true },
        { name: 'sent', data: at0(pts.map((p) => [p.t, p.sent_mbps / sc.div])), slot: 1, step: 'start' },
      ],
      xName: 'seconds',
      xMin: 0,
      yName: sc.unit,
      yFmt: (y) => fmtMbps(y * sc.div),
      xFmt: second,
    });
    // Sent within 2% of the peak of produced every second: one line hides the other.
    const overlap = max > 0 && pts.every((p) => Math.abs(p.gen_mbps - p.sent_mbps) <= 0.02 * max);
    return { lag, rate, overlap };
  }, [pts, rpo, P]);

  const limit = set.limit_mbps ? `throttled to ${fmtMbps(set.limit_mbps)}` : 'no throttle';
  const overS = v.over_rpo_s < 10 ? f1(v.over_rpo_s) : f0(v.over_rpo_s);
  const dur = num(set.duration_s);

  return (
    <>
      <p className="cdp-overview">
        <strong>{cdpSource(v.direction)}.</strong> {fmtMbps(set.rate_mbps)} of changed data on average{v.auto_rate ? ' (set automatically)' : ''},{' '}
        {v.base_mbps > 0 ? `${fmtMbps(v.base_mbps)} between bursts` : 'none between bursts'} and {fmtMbps(v.peak_mbps)} during {num(set.burst_s)} s bursts every{' '}
        {num(set.period_s)} s; {set.streams ?? 4} connections, {limit}, target RPO {num(rpo)} s, {dur} s run.
      </p>
      <div className="tiles cdp-tiles">
        <Tile
          label="Worst lag"
          value={secs(v.max_lag_s)}
          status={<VerdictBadge v={v.verdict} none="no verdict" />}
          sub={`against a ${num(rpo)} s target RPO · p95 ${secs(v.p95_lag_s)}`}
        />
        <Tile
          label="Time within the RPO"
          value={withinText(v.within_rpo_pct)}
          sub={v.over_rpo_s > 0 ? `over the RPO for ${overS} s of the ${dur} s run` : `never over the RPO in the ${dur} s run`}
        />
        <Tile
          label="Path carried while changes waited"
          value={v.backlogged ? fmtMbps(v.sustained_mbps) : 'Kept up'}
          sub={
            v.backlogged
              ? `the most replication can count on; ${fmtMbps(v.throughput_mbps)} sent on average`
              : `changes never had to wait; ${fmtMbps(v.throughput_mbps)} sent on average`
          }
        />
      </div>
      {pts.length > 1 && (
        <div className="chart-stack">
          <Figure
            title="Replication lag (the RPO the replica had) against the target"
            option={charts.lag}
            height={220}
            note={v.backlogged ? undefined : 'The lag stayed under half a second: each change was sent almost as soon as it was made.'}
          />
          <Figure
            title="Changed data produced and sent, per second"
            option={charts.rate}
            height={230}
            note={
              'Produced is the simulated change rate with its write bursts; sent is what crossed the path. While less is sent than produced, changes wait and the lag grows.' +
              (charts.overlap ? ' Here sent matched produced every second, so its line covers the produced one.' : '')
            }
          />
        </div>
      )}
      <div className="cdp-summary">
        <DataTable
          head={['Measure', 'Value']}
          rows={[
            ['Worst lag', secs(v.max_lag_s)],
            ['Lag p95 / mean / at the end', `${f1(v.p95_lag_s)} / ${f1(v.mean_lag_s)} / ${f1(v.end_lag_s)} s`],
            ['Time within the RPO', `${withinText(v.within_rpo_pct)} (${v.over_rpo_s > 0 ? `over it for ${overS} s` : 'never over it'})`],
            ['Changed data produced / sent', `${f0(v.generated_mb)} / ${f0(v.sent_mb)} MB`],
            ['Average sent', fmtMbps(v.throughput_mbps)],
            ['Path carried while changes waited', v.backlogged ? fmtMbps(v.sustained_mbps) : 'never had to wait'],
            ['Retransmitted', isNum(v.retrans_pct) ? `${f2(v.retrans_pct)}%` : '—'],
            ['Round trip idle / during (p95)', `${fmtMs(v.idle_rtt_ms?.median)} / ${fmtMs(v.loaded_rtt_ms?.p95)}`],
          ]}
        />
      </div>
      {pts.length > 0 && (
        <details className="table-toggle">
          <summary>Every second ({pts.length})</summary>
          <DataTable
            head={['Second', 'Worst lag (s)', 'Produced', 'Sent']}
            numeric={[0, 1, 2, 3]}
            rows={pts.map((p) => [second(p.t), f2(p.lag_s), fmtMbps(p.gen_mbps), fmtMbps(p.sent_mbps)])}
          />
        </details>
      )}
      {(v.notes ?? []).map((n, i) => (
        <p key={i} className="muted">
          {n}
        </p>
      ))}
    </>
  );
}

/** One-line headline for the live step list. */
export function cdpHeadline(v: CDPResult): string {
  return `${verdictInfo(v.verdict).label.toLowerCase()} · worst lag ${secs(v.max_lag_s)} (RPO ${num(v.settings?.rpo_s ?? 15)} s)`;
}
