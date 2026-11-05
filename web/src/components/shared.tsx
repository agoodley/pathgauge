// Components and cached lookups shared by several pages.
import { useEffect, useMemo, useState } from 'react';
import { api } from '../lib/api';
import { gradeLabel, runLabel, runStatusInfo } from '../lib/domain';
import { fmtAgo, fmtMbps, fmtMs, fmtTime, isNum, cap } from '../lib/format';
import { Link } from '../lib/router';
import type { Agent, Profiles, Run } from '../lib/types';
import { usePalette } from '../lib/theme';
import { Badge, Empty, ScoreBadge } from './ui';
import { EChart, sparkOption } from './Chart';

let profilesCache: Promise<Profiles> | null = null;

/** GET /api/profiles once per page load. */
export function useProfiles(): Profiles | undefined {
  const [p, setP] = useState<Profiles>();
  useEffect(() => {
    if (!profilesCache) profilesCache = api.get<Profiles>('/api/profiles');
    let live = true;
    profilesCache
      .then((x) => live && setP(x))
      .catch(() => {
        profilesCache = null;
      });
    return () => {
      live = false;
    };
  }, []);
  return p;
}

export function testTitle(p: Profiles | undefined, t: string): string {
  return p?.titles[t] ?? t;
}

export function RunStatusBadge({ run }: { run: Pick<Run, 'status' | 'error'> }) {
  const s = runStatusInfo(run.status);
  return <Badge kind={s.kind} icon={s.icon} label={s.label} title={run.error || undefined} />;
}

export function GradeBadge({ grade }: { grade: string }) {
  if (!grade) return <span className="muted">—</span>;
  const g = gradeLabel(grade);
  return <Badge kind={g.kind} icon={g.icon} label={grade} title={`Bufferbloat grade ${grade}: ${g.label}`} />;
}

export function profileLabel(r: Pick<Run, 'profile' | 'options'>): string {
  const tests = r.options?.tests ?? [];
  if (r.profile === 'custom' && tests.length) {
    // "a+b" entries are groups: count their tests too.
    const groups = tests.filter((t) => t.includes('+')).length;
    const n = tests.reduce((a, t) => a + t.split('+').length, 0);
    return `custom (${n} tests${groups ? `, ${groups} ${groups === 1 ? 'group' : 'groups'}` : ''})`;
  }
  return r.profile ? cap(r.profile) : '—';
}

/** Table of runs; columns adapt to whether the agent is already known. */
export function RunsTable({
  runs,
  showAgent = true,
  empty = 'No runs yet.',
  selectable,
}: {
  runs: Run[];
  showAgent?: boolean;
  empty?: string;
  selectable?: { selected: Set<string>; toggle: (id: string) => void; max: number };
}) {
  if (!runs.length) return <Empty>{empty}</Empty>;
  return (
    <div className="table-wrap">
      <table className="data runs-table">
        <thead>
          <tr>
            {selectable && (
              <th scope="col">
                <span className="sr-only">Select</span>
              </th>
            )}
            <th scope="col">Started</th>
            {showAgent && <th scope="col">Agent</th>}
            <th scope="col">Profile</th>
            <th scope="col">Status</th>
            <th scope="col">Health</th>
            <th scope="col" className="num">
              Download
            </th>
            <th scope="col" className="num">
              Upload
            </th>
            <th scope="col" className="num">
              Idle RTT
            </th>
            <th scope="col">Bloat</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => {
            const checked = selectable?.selected.has(r.id) ?? false;
            return (
              <tr key={r.id} className={checked ? 'selected' : undefined}>
                {selectable && (
                  <td>
                    <input
                      type="checkbox"
                      aria-label={`Select run ${r.id}`}
                      checked={checked}
                      disabled={!checked && selectable.selected.size >= selectable.max}
                      onChange={() => selectable.toggle(r.id)}
                    />
                  </td>
                )}
                <td>
                  <Link to={`/runs/${r.id}`} title={`Run ${r.id}`}>
                    {fmtTime(r.started_at || r.created_at)}
                  </Link>
                  <div className="cell-sub">{r.trigger === 'manual' ? r.requested_by : r.trigger}</div>
                </td>
                {showAgent && <td>{r.agent_name ? <Link to={`/agents/${r.agent_id}`}>{r.agent_name}</Link> : runLabel(r)}</td>}
                <td>{profileLabel(r)}</td>
                <td>
                  <RunStatusBadge run={r} />
                </td>
                <td>{r.status === 'done' ? <ScoreBadge score={r.health_score} /> : <span className="muted">—</span>}</td>
                <td className="num">{isNum(r.down_mbps) && r.down_mbps > 0 ? fmtMbps(r.down_mbps) : '—'}</td>
                <td className="num">{isNum(r.up_mbps) && r.up_mbps > 0 ? fmtMbps(r.up_mbps) : '—'}</td>
                <td className="num">{isNum(r.rtt_ms) && r.rtt_ms > 0 ? fmtMs(r.rtt_ms) : '—'}</td>
                <td>
                  <GradeBadge grade={r.bloat_grade} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function Sparkline({ values, height = 44 }: { values: number[]; height?: number }) {
  const P = usePalette();
  const opt = useMemo(() => sparkOption(P, values, (v) => fmtMs(v)), [P, values]);
  if (values.length < 2) return <div className="spark-empty muted">No monitoring data in the last 24 h</div>;
  return <EChart option={opt} height={height} label="RTT p95 over the last 24 hours" />;
}

/** Agent <select>; optionally disables agents that cannot run tests. */
export function AgentSelect({
  agents,
  value,
  onChange,
  allowAll,
  requireOnline,
  id,
}: {
  agents: Agent[];
  value: string;
  onChange: (v: string) => void;
  allowAll?: string;
  requireOnline?: boolean;
  id?: string;
}) {
  return (
    <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
      {allowAll !== undefined && <option value="">{allowAll}</option>}
      {!allowAll && !value && <option value="">Choose an agent…</option>}
      {agents
        .filter((a) => !a.revoked)
        .map((a) => {
          const off = requireOnline && (!a.online || a.busy);
          return (
            <option key={a.id} value={a.id} disabled={off}>
              {a.name}
              {a.site ? ` (${a.site})` : ''}
              {requireOnline ? (a.busy ? ' — busy' : a.online ? '' : ' — offline') : ''}
            </option>
          );
        })}
    </select>
  );
}

export function LastSeen({ a }: { a: Agent }) {
  return <span title={fmtTime(a.last_seen)}>{a.online ? 'now' : fmtAgo(a.last_seen)}</span>;
}
