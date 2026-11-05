import { useMemo, useState, type ReactNode } from 'react';
import { gradeOrder, severity } from '../lib/domain';
import { f0, f2, fmtMbps, fmtMs, fmtPct, fmtTime, isNum } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { Link, useQuery } from '../lib/router';
import type { CompareItem, Finding, Run, Summary } from '../lib/types';
import { Badge, Button, Card, Empty, ErrorBox, Loading, PageHeader } from '../components/ui';
import { RunsTable } from '../components/shared';

type Better = 'higher' | 'lower' | 'grade' | 'none';

interface Metric {
  label: string;
  get: (s: Summary) => number | string | boolean | undefined;
  fmt: (v: number | string | boolean | undefined) => string;
  better: Better;
  /** Whether the run measured this at all (0 usually means "not measured"). */
  present?: (s: Summary) => boolean;
}

const num = (fn: (n: number) => string) => (v: number | string | boolean | undefined) => (isNum(v) && v !== 0 ? fn(v) : '—');

const METRICS: Metric[] = [
  { label: 'Health score', get: (s) => s.health_score, fmt: (v) => (isNum(v) ? f0(v) : '—'), better: 'higher', present: () => true },
  { label: 'Download', get: (s) => s.down_mbps, fmt: num(fmtMbps), better: 'higher' },
  { label: 'Upload', get: (s) => s.up_mbps, fmt: num(fmtMbps), better: 'higher' },
  { label: 'Download, single stream', get: (s) => s.down_single_mbps, fmt: num(fmtMbps), better: 'higher' },
  { label: 'Upload, single stream', get: (s) => s.up_single_mbps, fmt: num(fmtMbps), better: 'higher' },
  { label: 'Idle RTT', get: (s) => s.idle_rtt_ms, fmt: num((n) => fmtMs(n)), better: 'lower' },
  { label: 'Jitter', get: (s) => s.jitter_ms, fmt: (v) => (isNum(v) ? fmtMs(v) : '—'), better: 'lower', present: (s) => s.idle_rtt_ms > 0 },
  { label: 'Loss', get: (s) => s.loss_pct, fmt: (v) => (isNum(v) ? fmtPct(v) : '—'), better: 'lower', present: (s) => s.idle_rtt_ms > 0 },
  { label: 'Bufferbloat grade', get: (s) => s.bloat_grade, fmt: (v) => (v ? String(v) : '—'), better: 'grade' },
  { label: 'Responsiveness (RPM)', get: (s) => s.rpm, fmt: num((n) => f0(n)), better: 'higher' },
  { label: 'Voice MOS', get: (s) => s.mos, fmt: num((n) => f2(n)), better: 'higher' },
  { label: 'Path MTU', get: (s) => s.path_mtu, fmt: num((n) => f0(n)), better: 'none' },
  { label: 'Hops out / back', get: (s) => (s.hops_up || s.hops_down ? `${s.hops_up} / ${s.hops_down}` : undefined), fmt: (v) => (v ? String(v) : '—'), better: 'none' },
  { label: 'NAT', get: (s) => s.nat, fmt: (v) => (v ? 'yes' : 'no'), better: 'none', present: (s) => s.hops_up > 0 || s.hops_down > 0 || s.path_mtu > 0 },
];

function isPresent(m: Metric, s: Summary): boolean {
  if (m.present) return m.present(s);
  const v = m.get(s);
  return v !== undefined && v !== '' && v !== 0 && v !== false;
}

/** Colour-free change label: arrow for the direction of change, words for better/worse. */
function delta(m: Metric, base: Summary, cur: Summary): ReactNode {
  const pa = isPresent(m, base);
  const pb = isPresent(m, cur);
  if (!pa && !pb) return null;
  if (!pa) return <span className="delta">not in baseline</span>;
  if (!pb) return <span className="delta">not measured</span>;
  const a = m.get(base);
  const b = m.get(cur);
  if (m.better === 'grade') {
    const ia = gradeOrder.indexOf(String(a));
    const ib = gradeOrder.indexOf(String(b));
    if (ia < 0 || ib < 0) return null;
    if (ia === ib) return <span className="delta">= same</span>;
    return ib < ia ? <span className="delta delta-better">↑ better</span> : <span className="delta delta-worse">↓ worse</span>;
  }
  if (m.better === 'none') {
    if (a === b || a === undefined || b === undefined) return null;
    return <span className="delta">≠ changed</span>;
  }
  if (!isNum(a) || !isNum(b)) return null;
  if (a === 0 && b === 0) return <span className="delta">= same</span>;
  const diff = b - a;
  const rel = a !== 0 ? diff / Math.abs(a) : Infinity;
  if (Math.abs(rel) < 0.01 || Math.abs(diff) < 1e-9) return <span className="delta">≈ same</span>;
  const up = diff > 0;
  const better = m.better === 'higher' ? up : !up;
  const pct = Number.isFinite(rel) ? `${up ? '+' : '−'}${Math.abs(rel * 100) >= 100 ? f0(Math.abs(rel * 100)) : Math.abs(rel * 100).toFixed(1)}%` : up ? 'new' : '';
  return (
    <span className={`delta ${better ? 'delta-better' : 'delta-worse'}`}>
      {up ? '↑' : '↓'} {pct} · {better ? 'better' : 'worse'}
    </span>
  );
}

export function Compare() {
  useDocumentTitle('Compare');
  const [q, setQ] = useQuery();
  const ids = (q.get('runs') ?? '').split(',').filter(Boolean).slice(0, 4);
  const ready = ids.length >= 2;
  const cmp = useApi<CompareItem[]>(ready ? `/api/compare?runs=${ids.map(encodeURIComponent).join(',')}` : null);
  const runs = useApi<Run[]>('/api/runs?status=done&limit=200');
  const [picking, setPicking] = useState(!ready);
  const [sel, setSel] = useState<Set<string>>(() => new Set(ids));

  const toggle = (id: string) => {
    const n = new Set(sel);
    if (n.has(id)) n.delete(id);
    else if (n.size < 4) n.add(id);
    setSel(n);
  };

  const apply = () => {
    // Keep chronological order: the oldest run is the baseline.
    const order = new Map((runs.data ?? []).map((r) => [r.id, r.started_at || r.created_at]));
    const list = [...sel].sort((a, b) => String(order.get(a) ?? '').localeCompare(String(order.get(b) ?? '')));
    setQ({ runs: list.join(',') });
    setPicking(false);
  };

  return (
    <div className="page">
      <PageHeader
        title="Compare runs"
        sub="Pick 2–4 runs (same or different agents). The first run is the baseline; changes are shown against it."
        actions={
          !picking && (
            <Button
              onClick={() => {
                setSel(new Set(ids));
                setPicking(true);
              }}
            >
              Change selection
            </Button>
          )
        }
      />
      {picking && (
        <Card
          title={`Choose runs (${sel.size} of 4 selected)`}
          actions={
            <>
              {ready && (
                <Button variant="ghost" onClick={() => setPicking(false)}>
                  Cancel
                </Button>
              )}
              <Button variant="primary" disabled={sel.size < 2} onClick={apply}>
                Compare {sel.size >= 2 ? sel.size : ''} runs
              </Button>
            </>
          }
        >
          <ErrorBox error={runs.error} onRetry={runs.reload} />
          {!runs.data && !runs.error && <Loading />}
          {runs.data && <RunsTable runs={runs.data} empty="No completed runs to compare." selectable={{ selected: sel, toggle, max: 4 }} />}
        </Card>
      )}
      {ready && !picking && (
        <>
          <ErrorBox error={cmp.error} onRetry={cmp.reload} />
          {!cmp.data && !cmp.error && <Loading />}
          {cmp.data && <CompareView items={cmp.data} />}
        </>
      )}
      {!ready && !picking && <Empty>Select at least two runs.</Empty>}
    </div>
  );
}

function CompareView({ items }: { items: CompareItem[] }) {
  const base = items[0];
  const findings = useMemo(() => findingsDiff(items), [items]);
  return (
    <>
      <Card title="Key metrics">
        <div className="table-wrap">
          <table className="data compare">
            <thead>
              <tr>
                <th scope="col">Metric</th>
                {items.map((it, i) => (
                  <th scope="col" key={it.id}>
                    <Link to={`/runs/${it.id}`}>{it.agent || 'imported'}</Link>
                    <div className="cell-sub">
                      {fmtTime(it.started_at)} · {it.profile}
                      {i === 0 ? ' · baseline' : ''}
                    </div>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {METRICS.map((m) => (
                <tr key={m.label}>
                  <th scope="row">
                    {m.label}
                    <div className="cell-sub">{m.better === 'higher' ? 'higher is better' : m.better === 'lower' ? 'lower is better' : m.better === 'grade' ? 'A+ is best' : ''}</div>
                  </th>
                  {items.map((it, i) => (
                    <td key={it.id}>
                      <div className="cmp-value">{isPresent(m, it.summary) ? m.fmt(m.get(it.summary)) : '—'}</div>
                      {i > 0 && delta(m, base.summary, it.summary)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
      <Card title="Findings">
        {findings.length === 0 ? (
          <Empty>None of these runs has findings.</Empty>
        ) : (
          <div className="table-wrap">
            <table className="data">
              <thead>
                <tr>
                  <th scope="col">Finding</th>
                  {items.map((it, i) => (
                    <th scope="col" key={it.id}>
                      {it.agent || 'imported'}
                      <div className="cell-sub">
                        {fmtTime(it.started_at)}
                        {i === 0 ? ' · baseline' : ''}
                      </div>
                    </th>
                  ))}
                  <th scope="col">Change (last vs baseline)</th>
                </tr>
              </thead>
              <tbody>
                {findings.map((row) => (
                  <tr key={row.id}>
                    <th scope="row">
                      {row.title}
                      <div className="cell-sub">
                        <code className="small">{row.id}</code>
                      </div>
                    </th>
                    {row.per.map((f, i) => (
                      <td key={i}>{f ? <Badge kind={severity(f.severity).kind} icon={severity(f.severity).icon} label={severity(f.severity).label} title={f.title} /> : <span className="muted">— absent</span>}</td>
                    ))}
                    <td>{row.change}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}

function findingsDiff(items: CompareItem[]) {
  const ids: string[] = [];
  const byRun = items.map((it) => {
    const m = new Map<string, Finding>();
    for (const f of it.findings ?? []) {
      if (!m.has(f.id)) m.set(f.id, f);
      if (!ids.includes(f.id)) ids.push(f.id);
    }
    return m;
  });
  const rows = ids.map((id) => {
    const per = byRun.map((m) => m.get(id));
    const first = per[0];
    const last = per[per.length - 1];
    const title = (last ?? per.find(Boolean))?.title ?? id;
    let change: string;
    if (first && !last) change = '✔ Resolved';
    else if (!first && last) change = '✚ New';
    else if (first && last && first.severity !== last.severity)
      change = severity(last.severity).rank < severity(first.severity).rank ? `↓ Worse (${severity(first.severity).label} → ${severity(last.severity).label})` : `↑ Better (${severity(first.severity).label} → ${severity(last.severity).label})`;
    else if (first && last) change = '= Unchanged';
    else change = 'Only in between';
    const rank = Math.min(...per.filter(Boolean).map((f) => severity((f as Finding).severity).rank));
    return { id, title, per, change, rank };
  });
  return rows.sort((a, b) => a.rank - b.rank);
}
