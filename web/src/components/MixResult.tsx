// Renderers for the traffic-mix and call-capacity tests, mirroring
// internal/report/mix.go so the dashboard and the HTML report agree.
import { useMemo, type ReactNode } from 'react';
import { verdictInfo } from '../lib/domain';
import { f0, f1, f2, fmtMbps, fmtMs, isNum, plural } from '../lib/format';
import { MIX_SCENARIOS } from '../lib/mix';
import { usePalette, type Palette } from '../lib/theme';
import type { CapacityResult, CapacitySearch, CapacityStep, MixFlow, MixKind, MixPhase, MixResult, MixSession } from '../lib/types';
import { barOption, Figure, lineOption, type Option } from './Chart';
import { Badge, DataTable, Tile } from './ui';

/** A verdict as icon + word; transfers (no verdict) get a plain note. */
export function VerdictBadge({ v, none = 'not graded' }: { v: string | null | undefined; none?: string }) {
  if (!v) return <span className="muted nowrap">{none}</span>;
  const i = verdictInfo(v);
  return <Badge kind={i.kind} icon={i.icon} label={i.label} />;
}

const lcFirst = (s: string) => (s ? s[0].toLowerCase() + s.slice(1) : s);
const ucFirst = (s: string) => (s ? s[0].toUpperCase() + s.slice(1) : s);

/** Categorical slot of a mix phase: colour follows the phase, not its index. */
const phaseSlot = (name: string) => (name === 'with-transfers' ? 1 : 0);

function scenarioLabel(id: string): string {
  return MIX_SCENARIOS.find((s) => s.id === id)?.label ?? id;
}

/** A kind's headline value with its unit (MixKind.value / unit). */
function kindValue(k: MixKind): string {
  const v = k.value;
  if (!isNum(v)) return '—';
  switch (k.unit) {
    case '':
      return f2(v);
    case '%':
      return `${f1(v)}%`;
    case 'ms':
      return v >= 100 ? `${f0(v)} ms` : `${f1(v)} ms`;
    case 'Mbit/s':
      return fmtMbps(v);
  }
  return k.unit.startsWith('%') ? `${f0(v)}${k.unit}` : `${v} ${k.unit}`;
}

/** One direction of a real-time session (same wording as the report). */
function flowText(kind: string, f: MixFlow | null | undefined): string {
  if (!f) return '—';
  if (kind.startsWith('voice')) return `MOS ${f2(f.mos)} · loss ${f1(f.loss_pct)}% · late ${f1(f.late_pct ?? 0)}% · +${f0(f.playout_ms ?? 0)} ms delay`;
  if (f.frames) return `${f1(f.frames.ok_pct)}% frames on time · ${plural(f.frames.freezes, 'freeze')} · +${f0(f.frames.playout_ms)} ms delay`;
  return `+${f0(f.added_p95_ms)} ms p95 · loss ${f1(f.loss_pct)}% · jitter ${f1(f.jitter_ms)} ms`;
}

/** Milliseconds as "850 ms" or "2.4 s" (fast paths load pages in a few ms). */
function fmtSecs(ms: number): string {
  return ms < 1000 ? `${f0(ms)} ms` : `${(ms / 1000).toFixed(1)} s`;
}

/** A web, streaming or transfer session in one line. */
function sessionText(s: MixSession): string {
  const failed = s.errors ? ` · ${s.errors} failed` : '';
  const t = s.times_ms;
  if (t && s.target_ms) return `${plural(s.count ?? 0, 'chunk')} · p95 ${fmtSecs(t.p95)} for ${f0(s.target_ms / 1000)} s of video${failed}`;
  if (t) return `${plural(s.count ?? 0, 'page')} · median ${fmtSecs(t.median)} · p95 ${fmtSecs(t.p95)}${failed}`;
  if (isNum(s.mbps) && s.mbps > 0) return fmtMbps(s.mbps);
  return '—';
}

/** Flow cell: the summary, plus the direction's verdict when it is not good. */
function FlowCell({ kind, f }: { kind: string; f: MixFlow | null | undefined }) {
  return (
    <>
      {flowText(kind, f)}
      {f && f.verdict && f.verdict !== 'good' && (
        <>
          {' '}
          <VerdictBadge v={f.verdict} />
        </>
      )}
    </>
  );
}

function SessionTable({ sessions }: { sessions: MixSession[] }) {
  return (
    <div className="table-wrap">
      <table className="data mix-sessions">
        <thead>
          <tr>
            <th scope="col">Session</th>
            <th scope="col">Verdict</th>
            <th scope="col">Outbound</th>
            <th scope="col">Return</th>
          </tr>
        </thead>
        <tbody>
          {sessions.map((s) => {
            const rt = !!(s.up || s.down);
            return (
              <tr key={s.id}>
                <td className="nowrap">
                  <code className="small">{s.id}</code>
                </td>
                <td>
                  <VerdictBadge v={s.verdict} />
                </td>
                {rt && !s.error ? (
                  <>
                    <td className="mix-flow">
                      <FlowCell kind={s.kind} f={s.up} />
                    </td>
                    <td className="mix-flow">
                      <FlowCell kind={s.kind} f={s.down} />
                    </td>
                  </>
                ) : (
                  <td colSpan={2} className={s.error ? 'text-critical' : undefined}>
                    {s.error || sessionText(s)}
                  </td>
                )}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function pctGood(k: MixKind): number | null {
  return k.verdict && k.sessions > 0 ? (100 * k.good) / k.sessions : null;
}

function PhaseBlock({ p, many }: { p: MixPhase; many: boolean }) {
  const kinds = p.kinds ?? [];
  const sessions = p.sessions ?? [];
  return (
    <div className="mix-phase">
      <h4 className="mix-phase-head">
        <span>{many ? p.label : 'Results by kind'}</span> <VerdictBadge v={p.verdict} />
      </h4>
      <p className="muted small">
        {f0(p.duration_s)} s{p.with_transfers ? ' with file transfers running' : ''}; latency probes: median {fmtMs(p.probe_rtt_ms?.median)}, p95 {fmtMs(p.probe_rtt_ms?.p95)}.
      </p>
      <div className="mix-kinds">
        <DataTable
          head={['Kind', 'Sessions', 'Good / acceptable / poor', 'Good %', 'Headline', 'Verdict']}
          numeric={[1, 3]}
          rows={kinds.map((k) => {
            const pg = pctGood(k);
            return [
              k.label,
              k.sessions,
              k.verdict ? `${k.good} / ${k.acceptable} / ${k.poor}` : <span className="muted">—</span>,
              pg === null ? <span className="muted">—</span> : `${f0(pg)}%`,
              <>
                {ucFirst(k.metric)}: <strong>{kindValue(k)}</strong>
                {k.summary && <div className="cell-sub">{k.summary}</div>}
              </>,
              <VerdictBadge v={k.verdict} />,
            ];
          })}
        />
      </div>
      <details className="table-toggle">
        <summary>Every session ({sessions.length})</summary>
        <SessionTable sessions={sessions} />
      </details>
    </div>
  );
}

export function MixView({ v }: { v: MixResult }) {
  const P = usePalette();
  const phases = v.phases ?? [];
  const counts = v.counts ?? [];

  // Share of good sessions per kind and phase: one unit, one axis. Kinds
  // without a verdict (transfers) are left out.
  const chart = useMemo(() => {
    const cats: { kind: string; label: string }[] = [];
    for (const p of phases)
      for (const k of p.kinds ?? []) if (k.verdict && k.sessions > 0 && !cats.some((c) => c.kind === k.kind)) cats.push({ kind: k.kind, label: k.label });
    const series = phases.map((p) => ({
      name: p.label,
      slot: phaseSlot(p.name),
      values: cats.map((c) => {
        const k = (p.kinds ?? []).find((x) => x.kind === c.kind);
        return k ? pctGood(k) : null;
      }),
    }));
    const option: Option = barOption(P, {
      // Long labels put their detail on a second line: "Video call\n(720p, 1.5 Mbit/s)".
      categories: cats.map((c) => (c.label.length > 18 ? c.label.replace(' (', '\n(') : c.label)),
      series,
      xName: '',
      yName: '% of sessions good',
      yFmt: (y) => `${f0(y)}%`,
      yMax: 100,
      horizontal: true,
    });
    const bars = cats.length * Math.max(1, series.length);
    return { option, cats: cats.length, series: series.length, bars };
  }, [phases, P]);

  const dscp = v.dscp ? 'calls marked EF (voice) and AF41 (video)' : 'calls sent unmarked (no DSCP)';
  const list = counts.map((c) => `${c.count} × ${lcFirst(c.label)}`).join(', ');
  // Legend row + value axis + one band per kind, each wide enough for the bars.
  const height = (chart.series >= 2 ? 44 : 30) + 36 + chart.cats * (chart.series >= 2 ? 46 : 34);

  return (
    <>
      <p className="mix-overview">
        <span className="mix-overall">
          Overall <VerdictBadge v={v.verdict} none="no verdict" />
        </span>{' '}
        {v.scenario ? (
          <>
            Scenario <strong>{scenarioLabel(v.scenario)}</strong>:{' '}
          </>
        ) : (
          'Custom mix: '
        )}
        {list}; {dscp}. Idle round trip {fmtMs(v.base_rtt_ms)}.
      </p>
      {chart.bars >= 2 && (
        <Figure
          title="Sessions with good quality, by kind"
          option={chart.option}
          height={Math.max(150, height)}
          note={phases.length > 1 ? 'The mix runs twice: first without the file transfers, then with them, to show what the transfers do to everything else.' : undefined}
        />
      )}
      {phases.map((p) => (
        <PhaseBlock key={p.name} p={p} many={phases.length > 1} />
      ))}
      {(v.notes ?? []).map((n, i) => (
        <p key={i} className="muted">
          {n}
        </p>
      ))}
    </>
  );
}

// ---- call capacity ----

interface CapMetric {
  /** Column and axis label. */
  label: string;
  axis: string;
  title: string;
  fmt: (v: number) => string;
  yMin?: number;
  yMax?: number;
}

/** What CapacityStep.worst means for a session kind. */
export function capacityMetric(kind: string): CapMetric {
  if (kind.startsWith('video'))
    return {
      label: 'Worst frames on time',
      axis: 'worst frames on time (%)',
      title: 'Worst share of video frames on time, by number of simultaneous calls',
      fmt: (v) => `${f1(v)}%`,
      yMin: 0,
      yMax: 100,
    };
  if (kind === 'gaming')
    return {
      label: 'Worst added delay',
      axis: 'worst added delay p95 (ms)',
      title: 'Worst added delay (p95), by number of simultaneous game sessions',
      fmt: (v) => fmtMs(v, 1),
    };
  return { label: 'Worst MOS', axis: 'worst MOS', title: 'Worst call quality (MOS), by number of simultaneous calls', fmt: (v) => f2(v), yMin: 1, yMax: 5 };
}

export const SEARCH_LABEL: Record<string, string> = { alone: 'On their own', 'with-transfers': 'With a download and an upload' };
const SEARCH_SLOT: Record<string, number> = { alone: 0, 'with-transfers': 1 };

/**
 * True when the two searches measured the same worst value at a shared
 * session count, so one line hides the other there.
 */
export function capacityOverlap(searches: { search: string; steps: CapacityStep[] }[]): boolean {
  const [a, b] = searches.map((s) => new Map(s.steps.map((x) => [x.sessions, x.worst])));
  if (!a || !b) return false;
  for (const [n, w] of a) if (b.has(n) && Math.abs((b.get(n) ?? NaN) - w) < 1e-9) return true;
  return false;
}

/** Note under a capacity chart whose lines coincide. */
export const OVERLAP_NOTE = 'Where the lines overlap both searches measured the same value; the larger dots are the calls on their own.';

/** Worst metric against session count, one series per search, one y-axis. */
export function capacityOption(P: Palette, kind: string, searches: { search: string; steps: CapacityStep[] }[]): Option {
  const m = capacityMetric(kind);
  const max = Math.max(1, ...searches.flatMap((s) => s.steps.map((x) => x.sessions)));
  let yMin = m.yMin;
  if (kind.startsWith('video')) {
    // The verdicts turn at 99% and 95% of frames on time: from zero those
    // differences vanish, so start the (line) axis just below the data.
    const lo = Math.min(100, ...searches.flatMap((s) => s.steps.map((x) => x.worst)));
    yMin = Math.max(0, Math.min(80, Math.floor((lo - 5) / 10) * 10));
  }
  return lineOption(P, {
    series: searches
      .filter((s) => s.steps.length)
      .map((s) => ({
        name: SEARCH_LABEL[s.search] ?? s.search,
        slot: SEARCH_SLOT[s.search] ?? 2,
        markers: true,
        // Drawn first and larger, so a coinciding second series leaves a ring.
        markerSize: s.search === 'alone' ? 14 : 6,
        data: [...s.steps].sort((a, b) => a.sessions - b.sessions).map((x) => [x.sessions, x.worst] as [number, number]),
      })),
    // The search doubles the count, so a log axis spaces its steps evenly.
    xLog: max >= 8,
    xInteger: true,
    xName: kind.startsWith('voice') || kind.startsWith('video') ? 'simultaneous calls' : 'simultaneous sessions',
    yName: m.axis,
    yMin,
    yMax: m.yMax,
    yFmt: m.fmt,
    xFmt: (x) => plural(x, 'session'),
  });
}

/** The steps of one search (the live page passes them in arrival order). */
export function CapacityStepsTable({ kind, steps }: { kind: string; steps: CapacityStep[] }) {
  const m = capacityMetric(kind);
  return (
    <DataTable
      head={['Sessions', 'Good / acceptable / poor', m.label, 'Worst loss %', 'Worst added delay p95', 'Verdict']}
      numeric={[0, 2, 3, 4]}
      rows={steps.map((s) => [
        s.sessions,
        `${s.good} / ${s.acceptable} / ${s.poor}`,
        m.fmt(s.worst),
        f2(s.loss_pct),
        fmtMs(s.added_p95_ms, 1),
        <VerdictBadge v={s.verdict} />,
      ])}
    />
  );
}

const callNoun = (kind: string) => (kind.startsWith('voice') || kind.startsWith('video') ? 'calls' : 'sessions');

function SearchTile({ label, kind, s }: { label: string; kind: string; s: CapacitySearch }) {
  const noun = callNoun(kind);
  let sub: ReactNode;
  if (s.acceptable === 0) sub = `Even one ${noun.replace(/s$/, '')} was poor quality.`;
  else
    sub = (
      <>
        {noun} at acceptable quality or better; {s.good === s.acceptable ? 'all of them good' : s.good === 0 ? 'never all good' : `all good up to ${s.good}`}.
        {s.limit ? (
          <>
            <br />
            Stopped at the {s.limit_reason || 'limit'} before quality dropped.
          </>
        ) : null}
      </>
    );
  return (
    <Tile
      label={label}
      value={
        <>
          {s.limit ? <span className="tile-prefix">at least </span> : null}
          {s.acceptable}
        </>
      }
      sub={sub}
    />
  );
}

export function CapacityView({ v }: { v: CapacityResult }) {
  const P = usePalette();
  const searches = useMemo(() => {
    const out: { search: string; title: string; s: CapacitySearch }[] = [{ search: 'alone', title: 'On their own', s: v.alone }];
    if (v.with_transfers) out.push({ search: 'with-transfers', title: 'While a download and an upload run', s: v.with_transfers });
    return out;
  }, [v]);
  const series = useMemo(() => searches.map((x) => ({ search: x.search, steps: x.s?.steps ?? [] })), [searches]);
  const option = useMemo(() => capacityOption(P, v.kind, series), [P, v.kind, series]);
  const m = capacityMetric(v.kind);
  const points = searches.reduce((n, x) => n + (x.s?.steps ?? []).length, 0);
  return (
    <>
      <div className="tiles capacity-tiles">
        {searches.map((x) => (
          <SearchTile key={x.search} label={x.title} kind={v.kind} s={x.s} />
        ))}
      </div>
      <p className="muted">
        {v.label}, about {v.per_session_kbps >= 1000 ? fmtMbps(v.per_session_kbps / 1000) : `${f0(v.per_session_kbps)} kbit/s`} each way per session; {f0(v.step_s)} s per step. The search doubles the number of {callNoun(v.kind)} until one
        is poor, then narrows the gap.
      </p>
      {points >= 2 && <Figure title={m.title} option={option} height={240} note={capacityOverlap(series) ? OVERLAP_NOTE : undefined} />}
      {searches.map((x) => (
        <div key={x.search}>
          <h4>{x.title}</h4>
          <CapacityStepsTable kind={v.kind} steps={[...(x.s?.steps ?? [])].sort((a, b) => a.sessions - b.sessions)} />
        </div>
      ))}
    </>
  );
}

// ---- one-line headlines for the live step list ----

export function mixHeadline(v: MixResult): string {
  const n = (v.counts ?? []).reduce((a, c) => a + c.count, 0);
  return `${verdictInfo(v.verdict).label.toLowerCase()} overall · ${plural(n, 'session')}`;
}

export function capacityHeadline(v: CapacityResult): string {
  const c = (s: CapacitySearch) => (s.limit ? `≥ ${s.acceptable}` : String(s.acceptable));
  const parts = [`${c(v.alone)} ${callNoun(v.kind)} alone`];
  if (v.with_transfers) parts.push(`${c(v.with_transfers)} with transfers`);
  return parts.join(' · ');
}
