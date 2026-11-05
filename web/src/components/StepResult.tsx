// Renders each typed step result with charts and data tables, mirroring
// internal/report/html.go so the dashboard and the HTML report agree.
import { useMemo, type ReactNode } from 'react';
import { usePalette, type Palette } from '../lib/theme';
import { f1, f2, f0, fmtBytes, fmtMbps, fmtMs, fmtPct, isNum, rateScale } from '../lib/format';
import type {
  BloatResult,
  CCResult,
  DiscriminationResult,
  HostReport,
  InfoResult,
  IntegrityResult,
  LatencyResult,
  PMTUDir,
  PMTUResult,
  QoSResult,
  ShortFlowResult,
  SizeSweepResult,
  SQMResult,
  StatSummary,
  Step,
  StreamsResult,
  SweepResult,
  TCPResult,
  TraceResult,
  UDPResult,
  VoIPResult,
} from '../lib/types';
import { barOption, Figure, lineOption, type LineSeries } from './Chart';
import { Badge, DataTable } from './ui';
import { CheckBadge, HostSummaryLine } from './HostDiag';
import { GradeBadge } from './shared';

const sumHead = ['Measure', 'Samples', 'Min', 'Median', 'Mean', 'p95', 'p99', 'Max', 'Std dev'];
function sumRow(label: string, s: StatSummary | null | undefined): ReactNode[] {
  const x = s ?? ({} as Partial<StatSummary>);
  return [label, x.n ?? 0, f2(x.min), f2(x.median), f2(x.mean), f2(x.p95), f2(x.p99), f2(x.max), f2(x.stddev)];
}

const dirName = (d: string) => ({ up: 'upload', down: 'download', bidir: 'both directions' })[d] ?? d;

export function StepSection({ step, title }: { step: Step; title?: string }) {
  const statusBadge =
    step.status === 'ok' ? (
      <Badge kind="good" icon="✔" label="OK" />
    ) : step.status === 'skipped' ? (
      <Badge kind="neutral" icon="–" label="Skipped" />
    ) : (
      <Badge kind="critical" icon="✖" label="Error" />
    );
  return (
    <section className="card step" id={`step-${step.id}`} aria-labelledby={`step-h-${step.id}`}>
      <header className="card-head">
        <h3 id={`step-h-${step.id}`}>
          {title ?? step.title}{' '}
          <small className="muted">
            {step.id} · {f1(step.seconds)} s
          </small>
        </h3>
        {statusBadge}
      </header>
      {step.status !== 'ok' ? (
        <p className="step-error">
          {step.status}: {step.error || 'no result'}
        </p>
      ) : (
        <StepBody step={step} />
      )}
    </section>
  );
}

export function StepBody({ step }: { step: Step }) {
  const r = step.result;
  if (!r) return <p className="muted">No result recorded.</p>;
  switch (step.test) {
    case 'info':
      return <InfoView v={r as InfoResult} />;
    case 'latency':
      return <LatencyView v={r as LatencyResult} />;
    case 'tcp':
      return <TCPView v={r as TCPResult} />;
    case 'udp':
      return <UDPView v={r as UDPResult} />;
    case 'streams':
      return <StreamsView v={r as StreamsResult} />;
    case 'bufferbloat':
      return <BloatView v={r as BloatResult} />;
    case 'trace':
    case 'mtr':
      return <TraceView v={r as TraceResult} />;
    case 'pmtu':
      return <PMTUView v={r as PMTUResult} />;
    case 'integrity':
      return <IntegrityView v={r as IntegrityResult} />;
    case 'voip':
      return <VoIPView v={r as VoIPResult} />;
    case 'sweep':
      return <SweepView v={r as SweepResult} />;
    case 'sizesweep':
      return <SizeSweepView v={r as SizeSweepResult} />;
    case 'cc':
      return <CCView v={r as CCResult} />;
    case 'shortflow':
      return <ShortFlowView v={r as ShortFlowResult} />;
    case 'discrimination':
      return <DiscriminationView v={r as DiscriminationResult} />;
    case 'sqm':
      return <SQMView v={r as SQMResult} />;
    case 'qos':
      return <QoSView v={r as QoSResult} />;
  }
  return (
    <details>
      <summary>Raw result (no renderer for “{step.test}”)</summary>
      <pre className="pre">{JSON.stringify(r, null, 2)}</pre>
    </details>
  );
}

/** One-line headline of a finished step (for the live step list). */
export function stepHeadline(test: string, r: unknown): string {
  if (!r || typeof r !== 'object') return '';
  try {
    switch (test.replace(/-.*$/, '')) {
      case 'latency': {
        const v = r as LatencyResult;
        return `RTT ${fmtMs(v.udp_rtt_ms?.median)} · jitter ${fmtMs(v.udp_jitter_ms)} · loss ${fmtPct(v.loss?.loss_pct)}`;
      }
      case 'tcp':
        return fmtMbps((r as TCPResult).mbps);
      case 'udp': {
        const v = r as UDPResult;
        return `${fmtMbps(v.mbps)} · loss ${fmtPct(v.loss?.loss_pct, 3)}`;
      }
      case 'streams':
        return `best ${fmtMbps((r as StreamsResult).best_mbps)}`;
      case 'bufferbloat': {
        const v = r as BloatResult;
        return `grade ${v.grade} · ${f0(v.rpm)} RPM`;
      }
      case 'sweep':
        return `${(r as SweepResult).signature} · capacity ${fmtMbps((r as SweepResult).capacity_mbps)}`;
      case 'voip':
        return `MOS ${f2((r as VoIPResult).up?.mos)} / ${f2((r as VoIPResult).down?.mos)}`;
      case 'pmtu':
        return `MTU ${(r as PMTUResult).up?.mtu} / ${(r as PMTUResult).down?.mtu}`;
      case 'cc':
        return `best ${(r as CCResult).best}`;
      case 'discrimination':
        return (r as DiscriminationResult).verdict;
      case 'sizesweep':
        return `max ${f0((r as SizeSweepResult).max_pps)} packets/s`;
      case 'qos':
        return (r as QoSResult).priority_effective ? 'priority protects latency' : 'priority not effective';
    }
  } catch {
    return '';
  }
  return '';
}

// ---- per-type views ----

function HostBlock({ name, h }: { name: string; h: HostReport }) {
  const problems = (h.checks ?? []).filter((c) => c.status === 'warn' || c.status === 'missing');
  return (
    <div>
      <h4>
        {name}: {h.hostname}
      </h4>
      <HostSummaryLine h={h} />
      {problems.length > 0 && (
        <DataTable
          head={['Area', 'Check', 'Status', 'Detail', 'Fix']}
          rows={problems.map((c) => [c.section, c.name, <CheckBadge status={c.status} />, c.detail, c.fix ?? ''])}
        />
      )}
    </div>
  );
}

function InfoView({ v }: { v: InfoResult }) {
  return (
    <>
      {v.client_host && <HostBlock name="Client" h={v.client_host} />}
      {v.endpoint_host && <HostBlock name="Endpoint" h={v.endpoint_host} />}
      <DataTable
        head={['Clock', 'Synchronised', 'Estimated error']}
        rows={[
          ['Client', v.client_clock?.synced ? 'yes' : 'no', fmtMs(v.client_clock?.est_error_ms)],
          ['Endpoint', v.endpoint_clock?.synced ? 'yes' : 'no', fmtMs(v.endpoint_clock?.est_error_ms)],
        ]}
      />
      <p>
        Endpoint policy: up to {v.endpoint_policy?.max_rate_mbps} Mbit/s, {v.endpoint_policy?.max_test_duration_s} s per test, shaping{' '}
        {v.endpoint_policy?.shaping || 'none'}. Estimated clock offset between hosts: {fmtMs(v.clock_offset_ms)}. One-way delays reported:{' '}
        {v.owd_trustworthy ? 'yes' : 'no (clocks not synchronised well enough)'}.
      </p>
    </>
  );
}

function LatencyView({ v }: { v: LatencyResult }) {
  const P = usePalette();
  const opt = useMemo(() => {
    const interval = 1 / Math.max(v.rate_hz || 1, 1);
    const data: [number, number | null][] = (v.series_ms ?? []).map((y, i) => [Math.round(i * interval * 1000) / 1000, y < 0 ? null : y]);
    return lineOption(P, { series: [{ name: 'UDP RTT', data }], xName: 'seconds', yName: 'RTT (ms)', yFmt: (x) => fmtMs(x), xFmt: (x) => `${f2(x)} s` });
  }, [v, P]);
  const rows: ReactNode[][] = [sumRow('UDP round trip (ms)', v.udp_rtt_ms)];
  if (v.icmp_rtt_ms) rows.push(sumRow('ICMP ping (ms)', v.icmp_rtt_ms));
  if (v.tcp_connect_ms) rows.push(sumRow('TCP connect (ms)', v.tcp_connect_ms));
  if (v.owd_up_ms) rows.push(sumRow('One-way outbound (ms)', v.owd_up_ms));
  if (v.owd_down_ms) rows.push(sumRow('One-way return (ms)', v.owd_down_ms));
  return (
    <>
      <Figure title="Round-trip time of each probe (gaps are lost probes)" option={opt} height={220} />
      <DataTable head={sumHead} rows={rows} numeric={[1, 2, 3, 4, 5, 6, 7, 8]} />
      <p>
        Loss {fmtPct(v.loss?.loss_pct)} ({v.loss?.lost} of {v.loss?.sent}), {v.loss?.reordered} reordered, {v.loss?.duplicates} duplicated. Jitter (RFC 3550){' '}
        {fmtMs(v.udp_jitter_ms)} round trip, {fmtMs(v.up_jitter_ms)} outbound, {fmtMs(v.down_jitter_ms)} return.
        {v.icmp_note ? ` ${v.icmp_note}.` : ''}
      </p>
    </>
  );
}

function rateSeries(P: Palette, name: string, pts: [number, number][], xName = 'seconds', title?: string) {
  const max = Math.max(0, ...pts.map((p) => p[1]));
  const sc = rateScale(max);
  return {
    unit: sc.unit,
    option: lineOption(P, {
      series: [{ name: title ?? name, data: pts.map(([x, y]) => [x, y / sc.div]) }],
      xName,
      yName: sc.unit,
      yFmt: (y) => fmtMbps(y * sc.div),
      xFmt: (x) => `${f1(x)} s`,
    }),
  };
}

function TCPView({ v }: { v: TCPResult }) {
  const P = usePalette();
  const { thr, rtt } = useMemo(() => {
    const iv = v.intervals ?? [];
    return {
      thr: rateSeries(P, 'throughput', iv.map((i) => [i.t, i.mbps])).option,
      rtt: lineOption(P, {
        series: [{ name: 'RTT', data: iv.map((i) => [i.t, i.rtt_ms]), slot: 1 }],
        xName: 'seconds',
        yName: 'RTT (ms)',
        yFmt: (x) => fmtMs(x),
        xFmt: (x) => `${f1(x)} s`,
      }),
    };
  }, [v, P]);
  return (
    <>
      <div className="grid-2 tight">
        <Figure title={`Throughput, ${v.streams} stream(s), receiver side`} option={thr} height={210} />
        <Figure title="Sender's smoothed RTT during the test (queueing shows as a rise)" option={rtt} height={210} />
      </div>
      <DataTable
        head={['Goodput', 'Peak', 'Retransmits', 'Retrans %', 'RTT median', 'RTT p95', 'Window-limited', 'Send-buffer-limited', 'MSS', 'Ramp', 'CC', 'Fairness']}
        rows={[
          [
            fmtMbps(v.mbps),
            fmtMbps(v.peak_mbps),
            v.retransmits,
            isNum(v.retrans_pct) ? v.retrans_pct.toFixed(3) : '—',
            fmtMs(v.rtt_ms?.median),
            fmtMs(v.rtt_ms?.p95),
            `${f1(v.rwnd_limited_pct)}%`,
            `${f1(v.sndbuf_limited_pct)}%`,
            v.mss,
            `${f1(v.ramp_s)} s`,
            v.congestion,
            isNum(v.fairness) ? v.fairness.toFixed(3) : '—',
          ],
        ]}
      />
    </>
  );
}

function UDPView({ v }: { v: UDPResult }) {
  const P = usePalette();
  const { thr, q } = useMemo(() => {
    const bins = v.bins ?? [];
    return {
      thr: rateSeries(P, 'delivered', bins.map((b) => [b.t, b.mbps])).option,
      q: lineOption(P, {
        series: [{ name: 'queue delay', data: bins.map((b) => [b.t, b.queue_delay_ms]), slot: 1 }],
        xName: 'seconds',
        yName: 'delay (ms)',
        yFmt: (x) => fmtMs(x),
        xFmt: (x) => `${f1(x)} s`,
      }),
    };
  }, [v, P]);
  return (
    <>
      {(v.bins ?? []).length > 0 && (
        <div className="grid-2 tight">
          <Figure title={`Delivered rate of a ${f1(v.target_mbps)} Mbit/s UDP stream (${dirName(v.direction)})`} option={thr} height={200} />
          <Figure title="Queueing delay (one-way delay above its minimum)" option={q} height={200} />
        </div>
      )}
      <DataTable
        head={[
          'Target',
          'Delivered',
          'Packets/s',
          'Loss %',
          ...(v.host_drops ? ['Network loss %', 'Host drops'] : []),
          'Bursts (max)',
          'Reordered %',
          'Jitter',
          'Queue p50 / p95',
        ]}
        rows={[
          [
            fmtMbps(v.target_mbps),
            fmtMbps(v.mbps),
            f0(v.pps),
            isNum(v.loss?.loss_pct) ? v.loss.loss_pct.toFixed(3) : '—',
            ...(v.host_drops ? [isNum(v.net_loss_pct) ? v.net_loss_pct.toFixed(3) : '—', v.host_drops] : []),
            `${v.loss?.bursts ?? 0} (${v.loss?.max_burst ?? 0})`,
            f2(v.loss?.reorder_pct),
            fmtMs(v.jitter_ms),
            `${f2(v.queue_delay_ms?.median)} / ${f2(v.queue_delay_ms?.p95)} ms`,
          ],
        ]}
      />
      {!!v.host_drops && <p className="muted">The receiving host dropped {v.host_drops} packets itself (socket buffer full); network loss excludes them.</p>}
    </>
  );
}

function StreamsView({ v }: { v: StreamsResult }) {
  const P = usePalette();
  const pts = v.points ?? [];
  const opt = useMemo(() => {
    const sc = rateScale(Math.max(0, ...pts.map((p) => p.mbps)));
    return barOption(P, {
      categories: pts.map((p) => String(p.streams)),
      series: [{ name: 'throughput', values: pts.map((p) => p.mbps / sc.div) }],
      xName: 'streams',
      yName: sc.unit,
      yFmt: (y) => fmtMbps(y * sc.div),
    });
  }, [pts, P]);
  return (
    <>
      <Figure title={`Throughput by number of parallel streams (${dirName(v.direction)})`} option={opt} height={220} />
      <DataTable head={['Streams', 'Throughput', 'Retransmits', 'RTT']} numeric={[0, 1, 2, 3]} rows={pts.map((p) => [p.streams, fmtMbps(p.mbps), p.retransmits, fmtMs(p.rtt_ms)])} />
      <p>
        Best {fmtMbps(v.best_mbps)}; one stream achieves {f0(100 * (v.single_stream_efficiency || 0))}% of it.
      </p>
    </>
  );
}

const PHASE_LABEL: Record<string, string> = { idle: 'idle', down: 'download busy', up: 'upload busy', bidir: 'both busy' };
const PHASE_ORDER = ['idle', 'down', 'up', 'bidir'];

/**
 * One series per bufferbloat phase (idle, download, upload, both: four
 * fixed slots). Anything else, such as the "drain" pauses between phases,
 * folds into one neutral grey series so no fifth hue is introduced.
 */
export function bloatSeries(phases: { name: string; points: [number, number][] }[], otherColor: string): LineSeries[] {
  const known: LineSeries[] = phases
    .filter((p) => p.points.length && PHASE_ORDER.includes(p.name))
    .sort((a, b) => PHASE_ORDER.indexOf(a.name) - PHASE_ORDER.indexOf(b.name))
    .map((p) => ({
      name: PHASE_LABEL[p.name] ?? p.name,
      data: [...p.points].sort((a, b) => a[0] - b[0]),
      slot: PHASE_ORDER.indexOf(p.name),
      points: true,
    }));
  const other = phases.filter((p) => !PHASE_ORDER.includes(p.name)).flatMap((p) => p.points);
  if (other.length) {
    known.push({ name: 'between phases', data: [...other].sort((a, b) => a[0] - b[0]), points: true, color: otherColor });
  }
  return known;
}

function BloatView({ v }: { v: BloatResult }) {
  const P = usePalette();
  const phases = v.phases ?? [];
  const opt = useMemo(() => {
    const series = bloatSeries(
      phases.map((ph) => ({ name: ph.name, points: (ph.series ?? []).map((s) => [s.t, s.rtt_ms] as [number, number]) })),
      P.muted,
    );
    return lineOption(P, { series, xName: 'seconds', yName: 'RTT (ms)', yFmt: (x) => fmtMs(x), xFmt: (x) => `${f2(x)} s` });
  }, [phases, P]);
  return (
    <>
      <Figure title="Latency of a separate light flow while the link is idle and busy" option={opt} height={240} />
      <DataTable
        head={['Phase', 'Probe RTT median', 'Probe RTT p95', 'In-flow RTT median', 'TCP connect', 'Increase (p95)', 'Grade', 'Download', 'Upload']}
        rows={phases.map((ph) => [
          PHASE_LABEL[ph.name] ?? ph.name,
          fmtMs(ph.rtt_ms?.median),
          fmtMs(ph.rtt_ms?.p95),
          fmtMs(ph.in_flow_rtt_ms?.median),
          fmtMs(ph.connect_ms?.median),
          ph.name === 'idle' ? '' : fmtMs(v.increase_ms?.[ph.name], 1),
          ph.name === 'idle' ? '' : <GradeBadge grade={v.grades?.[ph.name] ?? ''} />,
          fmtMbps(ph.down_mbps),
          fmtMbps(ph.up_mbps),
        ])}
      />
      <p>
        Overall grade <GradeBadge grade={v.grade} />; responsiveness {f0(v.rpm)} RPM (working latency {fmtMs(v.working_latency_ms, 1)}); queue behaviour:{' '}
        {v.queueing || 'unclear'}.
      </p>
    </>
  );
}

function TraceView({ v }: { v: TraceResult }) {
  const P = usePalette();
  const hops = v.hops ?? [];
  const opt = useMemo(
    () =>
      lineOption(P, {
        series: [
          {
            name: 'median RTT',
            data: hops.map((h) => [h.ttl, h.received > 0 ? h.rtt_ms?.median ?? null : null]),
            points: false,
          },
        ],
        xName: 'hop (TTL)',
        xInteger: true,
        yName: 'RTT (ms)',
        yFmt: (x) => fmtMs(x),
        xFmt: (x) => `hop ${x}`,
      }),
    [hops, P],
  );
  return (
    <>
      <p className="muted">
        From {v.from} to {v.target}, method {v.method}, {v.cycles} cycle(s), {v.reached ? 'destination reached' : 'destination not reached'}, fingerprint{' '}
        <code className="small">{v.fingerprint}</code>.
      </p>
      {hops.length >= 2 && <Figure title="Median round-trip time to each hop" option={opt} height={190} />}
      <DataTable
        head={['Hop', 'Address', 'Name / network', 'Loss %', 'Best ms', 'Median ms', 'Worst ms', 'Notes']}
        numeric={[0, 3, 4, 5, 6]}
        rows={hops.map((h) => {
          const asn = h.asn ? (/^as/i.test(h.asn) ? h.asn : `AS${h.asn}`) : '';
          const net = [asn, h.org].filter(Boolean).join(' ');
          const name = (
            <>
              {h.name || (net ? '' : <span className="muted">—</span>)}
              {net && <div className={h.name ? 'cell-sub' : undefined}>{net}</div>}
            </>
          );
          const flags: string[] = [];
          if (h.private) flags.push('private');
          if (h.cgnat) flags.push('CGNAT');
          if ((h.addrs ?? []).length > 1) flags.push(`${h.addrs?.length} responders (ECMP)`);
          if ((h.mpls ?? []).length) flags.push('MPLS ' + (h.mpls ?? []).join('/'));
          if (h.dest) flags.push('destination');
          if ((v.rate_limited_hops ?? []).includes(h.ttl)) flags.push('rate-limited (loss not persistent)');
          if (v.loss_from_hop && h.ttl === v.loss_from_hop) flags.push('loss starts here');
          return [h.ttl, h.addr ? <code className="small">{h.addr}</code> : '*', name, f1(h.loss_pct), f2(h.rtt_ms?.min), f2(h.rtt_ms?.median), f2(h.rtt_ms?.max), flags.join(', ')];
        })}
      />
      {v.note && <p className="muted">{v.note}.</p>}
    </>
  );
}

function pmtuRow(label: string, d: PMTUDir | undefined): ReactNode[] {
  if (!d) return [label, '—', '—', '—', '—', ''];
  return [label, d.mtu, d.local_mtu, d.ptb_seen ? `yes${d.ptb_mtu ? ` (${d.ptb_mtu})` : ''}` : 'no', d.blackhole ? <Badge kind="critical" icon="✖" label="yes" /> : 'no', d.note ?? ''];
}

function PMTUView({ v }: { v: PMTUResult }) {
  return <DataTable head={['Direction', 'Path MTU', 'Local MTU', 'PTB received', 'Black hole', 'Note']} rows={[pmtuRow('Outbound', v.up), pmtuRow('Return', v.down)]} />;
}

function dscpStatus(s: string) {
  const k = s === 'preserved' ? 'good' : s === 'lost' ? 'critical' : s === 'bleached' || s === 'remarked' ? 'warning' : 'neutral';
  const icon = s === 'preserved' ? '✔' : s === 'lost' ? '✖' : s ? '▲' : '';
  return s ? <Badge kind={k} icon={icon} label={s} /> : '—';
}

function IntegrityView({ v }: { v: IntegrityResult }) {
  const up = v.dscp_up ?? [];
  const down = v.dscp_down ?? [];
  const recv = (n: number | undefined) => (n === undefined ? '—' : n < 0 ? 'lost' : n);
  return (
    <>
      {up.length > 0 && (
        <DataTable
          head={['Marking', 'DSCP sent', 'Received out', 'Outbound', 'Received back', 'Return']}
          rows={up.map((d, i) => [d.name, d.sent, recv(d.received), dscpStatus(d.status), recv(down[i]?.received), dscpStatus(down[i]?.status ?? '')])}
        />
      )}
      <DataTable
        head={['Check', 'Result']}
        rows={[
          ['ECN (ECT(0) marks)', `outbound ${v.ecn_up}, return ${v.ecn_down}`],
          [
            'TCP MSS',
            `client advertises ${v.client_adv_mss} (endpoint uses ${v.endpoint_snd_mss}); endpoint advertises ${v.endpoint_adv_mss} (client uses ${v.client_snd_mss}); clamped out=${v.mss_clamped_up} back=${v.mss_clamped_down}`,
          ],
          ['TCP options negotiated', (v.client_tcp_options ?? []).join(', ') + ((v.stripped_tcp_options ?? []).length ? ` (stripped: ${(v.stripped_tcp_options ?? []).join(', ')})` : '')],
          ['NAT', `${v.nat ? 'yes' : 'no'} (client ${v.client_local} seen as ${v.client_observed}; port preserved ${v.port_preserved}; CGNAT ${v.cgnat})`],
          ['Hops from TTL', `outbound ${v.hops_up} (TTL ${v.ttl_received_up}), return ${v.hops_down} (TTL ${v.ttl_received_down})`],
        ]}
      />
    </>
  );
}

function VoIPView({ v }: { v: VoIPResult }) {
  const row = (label: string, d: VoIPResult['up']) => [label, f2(d?.mos), f1(d?.r_factor), f2(d?.loss?.loss_pct), f2(d?.late_loss_pct), f2(d?.jitter_ms), d?.verdict ?? ''];
  return (
    <>
      <DataTable head={['Direction', 'MOS', 'R', 'Loss %', 'Late %', 'Jitter ms', 'Verdict']} numeric={[1, 2, 3, 4, 5]} rows={[row('Outbound', v.up), row('Return', v.down)]} />
      <p className="muted">
        {v.codec} for {f0(v.duration_s)} s{v.loaded ? ' while both directions were saturated' : ''}; one-way network delay ≈ {fmtMs(v.one_way_ms, 1)}. Overall: {v.verdict}.
      </p>
    </>
  );
}

function SweepView({ v }: { v: SweepResult }) {
  const P = usePalette();
  const steps = v.steps ?? [];
  const charts = useMemo(() => {
    const max = Math.max(0, ...steps.map((s) => Math.max(s.offered_mbps, s.delivered_mbps)));
    const sc = rateScale(max);
    const xFmt = (x: number) => `offered ${fmtMbps(x * sc.div)}`;
    const off = steps.map((s) => s.offered_mbps / sc.div);
    return {
      del: lineOption(P, {
        series: [
          { name: 'delivered', data: steps.map((s, i) => [off[i], s.delivered_mbps / sc.div]), points: false },
          { name: 'offered (ideal)', data: off.map((x) => [x, x]), slot: 2, dashed: true },
        ],
        xName: `offered (${sc.unit})`,
        yName: `delivered (${sc.unit})`,
        yFmt: (y) => fmtMbps(y * sc.div),
        xFmt,
      }),
      loss: lineOption(P, {
        series: [{ name: 'loss', data: steps.map((s, i) => [off[i], s.loss_pct]), slot: 1 }],
        xName: `offered (${sc.unit})`,
        yName: 'loss (%)',
        yFmt: (y) => fmtPct(y),
        xFmt,
      }),
      queue: lineOption(P, {
        series: [
          { name: 'median', data: steps.map((s, i) => [off[i], s.queue_p50_ms]) },
          { name: 'p95', data: steps.map((s, i) => [off[i], s.queue_p95_ms]), slot: 1 },
        ],
        xName: `offered (${sc.unit})`,
        yName: 'queue delay (ms)',
        yFmt: (y) => fmtMs(y),
        xFmt,
      }),
    };
  }, [steps, P]);
  return (
    <>
      <p>
        {v.signature === 'host-limited' ? <Badge kind="warning" icon="▲" label="host-limited" /> : <strong>{v.signature}</strong>}: {v.signature_detail}
      </p>
      <div className="multiples">
        <Figure title="Delivered vs offered rate" option={charts.del} height={220} />
        <Figure title="Loss vs offered rate" option={charts.loss} height={220} />
        <Figure title="Queueing delay vs offered rate" option={charts.queue} height={220} />
      </div>
      <SweepTable steps={steps} />
      <p className="muted">
        Capacity {fmtMbps(v.capacity_mbps)}, knee {fmtMbps(v.knee_mbps)}, queue onset {fmtMbps(v.queue_onset_mbps)}, max queue {fmtMs(v.max_queue_ms, 1)}, estimated buffer{' '}
        {fmtBytes(v.buffer_bytes)}.
      </p>
    </>
  );
}

export function SweepTable({ steps }: { steps: SweepResult['steps'] }) {
  const host = (steps ?? []).some((s) => (s.host_drop_pct ?? 0) > 0 || s.host_limited);
  return (
    <DataTable
      head={['Offered', 'Delivered', 'Loss %', ...(host ? ['Network loss %', 'Host drop %'] : []), 'Queue p50 ms', 'Queue p95 ms', 'Jitter ms', '1st / 2nd half']}
      numeric={host ? [0, 1, 2, 3, 4, 5, 6, 7] : [0, 1, 2, 3, 4, 5]}
      rows={(steps ?? []).map((s) => [
        <>
          {fmtMbps(s.offered_mbps)}
          {s.host_limited && (
            <div>
              <Badge kind="warning" icon="▲" label="host-limited" title="The hosts could not send or receive this rate; the network was not the limit" />
            </div>
          )}
        </>,
        fmtMbps(s.delivered_mbps),
        f2(s.loss_pct),
        ...(host ? [f2(s.net_loss_pct), f2(s.host_drop_pct)] : []),
        f1(s.queue_p50_ms),
        f1(s.queue_p95_ms),
        f2(s.jitter_ms),
        isNum(s.first_half_mbps) && (s.first_half_mbps > 0 || s.second_half_mbps > 0) ? `${fmtMbps(s.first_half_mbps)} / ${fmtMbps(s.second_half_mbps)}` : '—',
      ])}
    />
  );
}

function SizeSweepView({ v }: { v: SizeSweepResult }) {
  const P = usePalette();
  const pts = v.points ?? [];
  const opt = useMemo(
    () =>
      barOption(P, {
        categories: pts.map((p) => String(p.size)),
        series: [{ name: 'packets/s', values: pts.map((p) => p.pps) }],
        xName: 'packet size (bytes)',
        yName: 'packets/s',
        yFmt: (y) => `${f0(y)} packets/s`,
      }),
    [pts, P],
  );
  return (
    <>
      <Figure title={`Packets per second delivered at ${f0(v.rate_mbps)} Mbit/s, by packet size (${dirName(v.direction)})`} option={opt} height={220} />
      <DataTable head={['Size (bytes)', 'Delivered', 'Packets/s', 'Loss %']} numeric={[0, 1, 2, 3]} rows={pts.map((p) => [p.size, fmtMbps(p.delivered_mbps), f0(p.pps), f2(p.loss_pct)])} />
    </>
  );
}

function CCView({ v }: { v: CCResult }) {
  const P = usePalette();
  const pts = v.points ?? [];
  const opt = useMemo(() => {
    const ok = pts.filter((p) => !p.error);
    const sc = rateScale(Math.max(0, ...ok.map((p) => p.mbps)));
    return barOption(P, {
      categories: ok.map((p) => p.algorithm),
      series: [{ name: 'throughput', values: ok.map((p) => p.mbps / sc.div) }],
      xName: 'algorithm',
      yName: sc.unit,
      yFmt: (y) => fmtMbps(y * sc.div),
    });
  }, [pts, P]);
  return (
    <>
      {pts.some((p) => !p.error) && <Figure title={`Single-stream throughput by congestion-control algorithm (best: ${v.best || '—'})`} option={opt} height={200} />}
      <DataTable head={['Algorithm', 'Throughput', 'Retransmits', 'RTT', 'Error']} rows={pts.map((p) => [p.algorithm, fmtMbps(p.mbps), p.retransmits, fmtMs(p.rtt_ms), p.error ?? ''])} />
    </>
  );
}

function ShortFlowView({ v }: { v: ShortFlowResult }) {
  const P = usePalette();
  const rows: [string, StatSummary][] = [['TCP connect', v.connect_ms]];
  for (const k of ['10KB', '100KB', '1MB']) {
    const c = v.completion_ms?.[k];
    if (c) rows.push([`${k} transfer`, c]);
  }
  const opt = useMemo(
    () =>
      barOption(P, {
        categories: rows.map((r) => r[0]),
        series: [
          { name: 'median', values: rows.map((r) => r[1]?.median ?? null) },
          { name: 'p95', values: rows.map((r) => r[1]?.p95 ?? null), slot: 1 },
        ],
        xName: '',
        yName: 'ms',
        yFmt: (y) => fmtMs(y),
      }),
    // rows derive from v
    [v, P],
  );
  return (
    <>
      <Figure title={`Connection setup and transfer completion times (${v.count} transfers, ${v.errors} errors)`} option={opt} height={210} />
      <DataTable head={['Operation', 'Median ms', 'p95 ms']} numeric={[1, 2]} rows={rows.map(([k, s]) => [k, f2(s?.median), f2(s?.p95)])} />
    </>
  );
}

function DiscriminationView({ v }: { v: DiscriminationResult }) {
  const P = usePalette();
  const pts = v.points ?? [];
  const opt = useMemo(() => {
    const ok = pts.filter((p) => !p.error);
    const sc = rateScale(Math.max(0, ...ok.map((p) => p.mbps)));
    return barOption(P, {
      categories: ok.map((p) => p.label),
      series: [{ name: 'throughput', values: ok.map((p) => p.mbps / sc.div) }],
      xName: '',
      yName: sc.unit,
      yFmt: (y) => fmtMbps(y * sc.div),
      horizontal: true,
    });
  }, [pts, P]);
  return (
    <>
      {pts.some((p) => !p.error) && <Figure title="Throughput by traffic type" option={opt} height={Math.max(160, 40 + pts.length * 34)} />}
      <DataTable head={['Traffic', 'Throughput', 'Error']} rows={pts.map((p) => [p.label, fmtMbps(p.mbps), p.error ?? ''])} />
      <p>{v.verdict}.</p>
    </>
  );
}

function SQMView({ v }: { v: SQMResult }) {
  const P = usePalette();
  const steps = v.steps ?? [];
  const opt = useMemo(
    () =>
      lineOption(P, {
        series: [{ name: 'latency increase', data: steps.filter((s) => !s.error).map((s) => [s.percent, s.increase_ms]), points: false }],
        xName: 'shaped rate (% of measured capacity)',
        yName: 'p95 increase (ms)',
        yFmt: (y) => fmtMs(y),
        xFmt: (x) => `${f0(x)}%`,
      }),
    [steps, P],
  );
  return (
    <>
      <p className="muted">
        Interface {v.interface}; baseline {fmtMbps(v.baseline_down_mbps)} down / {fmtMbps(v.baseline_up_mbps)} up, grade {v.baseline_grade || '—'}.
      </p>
      {steps.some((s) => !s.error) && <Figure title="Latency increase under load at each shaping rate" option={opt} height={210} />}
      <DataTable
        head={['Shaped to', 'Qdisc', 'Shaped Mbit/s (down / up)', 'Measured Mbit/s', 'Increase', 'Grade', 'Error']}
        rows={steps.map((s) => [
          `${f1(s.percent)}%`,
          s.qdisc,
          `${f1(s.down_shaped_mbps)} / ${f1(s.up_shaped_mbps)}`,
          `${f1(s.down_measured_mbps)} / ${f1(s.up_measured_mbps)}`,
          fmtMs(s.increase_ms, 1),
          <GradeBadge grade={s.grade} />,
          s.error ?? '',
        ])}
      />
      {v.recommended && (
        <p>
          Recommended: <strong>{f0(v.recommended.percent)}%</strong> ({f1(v.recommended.down_shaped_mbps)} / {f1(v.recommended.up_shaped_mbps)} Mbit/s) with {v.recommended.qdisc}, grade{' '}
          {v.recommended.grade}.
        </p>
      )}
      {(v.tc_commands ?? []).length > 0 && <pre className="pre">{(v.tc_commands ?? []).join('\n')}</pre>}
      {v.note && <p className="muted">{v.note}</p>}
    </>
  );
}

function QoSView({ v }: { v: QoSResult }) {
  const P = usePalette();
  const cls = v.classes ?? [];
  const opt = useMemo(
    () =>
      barOption(P, {
        categories: cls.map((c) => c.name),
        series: [
          { name: 'idle median', values: cls.map((c) => c.idle_rtt_ms?.median ?? null) },
          { name: 'loaded p95', values: cls.map((c) => c.loaded_rtt_ms?.p95 ?? null), slot: 1 },
        ],
        xName: '',
        yName: 'RTT (ms)',
        yFmt: (y) => fmtMs(y),
      }),
    [cls, P],
  );
  return (
    <>
      {cls.length > 0 && <Figure title="Probe RTT by DSCP class: idle median vs p95 under load" option={opt} height={220} />}
      <DataTable
        head={['Class', 'DSCP', 'Idle median ms', 'Loaded median ms', 'Loaded p95 ms', 'Increase ms', 'Loss %']}
        numeric={[1, 2, 3, 4, 5, 6]}
        rows={cls.map((c) => [c.name, c.dscp, f2(c.idle_rtt_ms?.median), f2(c.loaded_rtt_ms?.median), f2(c.loaded_rtt_ms?.p95), f1(c.increase_ms), f2(c.loss_pct)])}
      />
      <p>
        {v.priority_effective ? (
          <Badge kind="good" icon="✔" label="Priority effective" />
        ) : (
          <Badge kind="warning" icon="▲" label="Priority not effective" />
        )}{' '}
        {v.verdict}
      </p>
      {(v.down_mbps > 0 || v.up_mbps > 0) && (
        <p className="muted">
          Load during the test: {fmtMbps(v.down_mbps)} download, {fmtMbps(v.up_mbps)} upload.
        </p>
      )}
    </>
  );
}
