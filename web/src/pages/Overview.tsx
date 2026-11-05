import { useApi, useDocumentTitle, useInterval, useTick } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { agentState } from '../lib/domain';
import { fmtAgo, fmtDuration, fmtMbps, fmtMs, fmtTime, between, isNum, plural } from '../lib/format';
import { Link } from '../lib/router';
import { useCan } from '../lib/auth';
import type { AlertEvent, Overview as OverviewData, OverviewLink, Run } from '../lib/types';
import { Badge, Card, Empty, ErrorBox, Loading, PageHeader, ScoreBadge, StatusDot } from '../components/ui';
import { GradeBadge, RunsTable, Sparkline, profileLabel } from '../components/shared';

export function Overview() {
  useDocumentTitle('Overview');
  const ov = useApi<OverviewData>('/api/overview');
  const canRun = useCan('operator');
  useLiveRefresh((m) => m.type === 'run_status' || m.type === 'agent_status' || m.type === 'alert', ov.reload);
  useInterval(ov.reload, 60000);

  const d = ov.data;
  return (
    <div className="page">
      <PageHeader
        title="Overview"
        sub={d ? `${d.server_name} · ${plural(d.links.length, 'agent')}` : undefined}
        actions={
          canRun ? (
            <Link to="/runs/new" className="btn btn-primary">
              New run
            </Link>
          ) : undefined
        }
      />
      <ErrorBox error={ov.error} onRetry={ov.reload} />
      {!d && !ov.error && <Loading />}
      {d && (
        <>
          {d.links.length === 0 ? (
            <Card>
              <Empty>
                No agents are enrolled yet. <Link to="/agents">Add an agent</Link> to start testing.
              </Empty>
            </Card>
          ) : (
            <div className="link-grid">
              {d.links.map((l) => (
                <LinkTile key={l.agent.id} l={l} />
              ))}
            </div>
          )}
          <div className="grid-2">
            <Card title="Running now">
              <RunningList runs={d.running} />
            </Card>
            <Card title="Firing alerts" actions={<Link to="/alerts">All alerts</Link>}>
              <FiringAlerts alerts={d.alerts} links={d.links} />
            </Card>
          </div>
          <Card title="Recent runs" actions={<Link to="/reports">All runs</Link>}>
            <RunsTable runs={d.recent} />
          </Card>
        </>
      )}
    </div>
  );
}

function LinkTile({ l }: { l: OverviewLink }) {
  const st = agentState(l.agent);
  const r = l.last_run;
  return (
    <Link to={`/agents/${l.agent.id}`} className="link-tile" aria-label={`${l.agent.name}: ${st.label}`}>
      <div className="link-tile-head">
        <div>
          <div className="link-tile-name">{l.agent.name}</div>
          <div className="cell-sub">{l.agent.site || 'No site set'}</div>
        </div>
        <StatusDot kind={st.kind} label={st.label} icon={st.icon} />
      </div>
      <div className="link-tile-metrics">
        <div>
          <span className="metric-label">Health</span>
          <ScoreBadge score={r?.health_score ?? null} />
        </div>
        <div>
          <span className="metric-label">Download</span>
          <span className="metric-value">{r && isNum(r.down_mbps) && r.down_mbps > 0 ? fmtMbps(r.down_mbps) : '—'}</span>
        </div>
        <div>
          <span className="metric-label">Upload</span>
          <span className="metric-value">{r && isNum(r.up_mbps) && r.up_mbps > 0 ? fmtMbps(r.up_mbps) : '—'}</span>
        </div>
        <div>
          <span className="metric-label">Bufferbloat</span>
          <GradeBadge grade={r?.bloat_grade ?? ''} />
        </div>
      </div>
      <div className="link-tile-spark">
        <span className="metric-label">
          RTT p95, last 24 h{l.latest_sample ? ` · now ${fmtMs(l.latest_sample.rtt_p95)}` : ''}
        </span>
        <Sparkline values={l.spark_rtt_p95} />
      </div>
      <div className="link-tile-foot">
        {l.alerts_firing > 0 ? (
          <Badge kind="warning" icon="▲" label={`${plural(l.alerts_firing, 'alert')} firing`} />
        ) : (
          <span className="muted">
            <span aria-hidden="true">✔</span> No alerts firing
          </span>
        )}
        <span className="muted">{r ? `Last run ${fmtAgo(r.ended_at || r.created_at)}` : 'No runs yet'}</span>
      </div>
    </Link>
  );
}

function RunningList({ runs }: { runs: Run[] }) {
  useTick(1000, runs.length > 0);
  if (!runs.length) return <Empty>No tests are running.</Empty>;
  return (
    <ul className="plain-list">
      {runs.map((r) => (
        <li key={r.id} className="running-item">
          <span className="spinner" aria-hidden="true" />
          <div>
            <Link to={`/runs/${r.id}`}>
              {r.agent_name || r.agent_id} · {profileLabel(r)}
            </Link>
            <div className="cell-sub">
              started {fmtTime(r.started_at || r.created_at)} · running for {fmtDuration(between(r.started_at || r.created_at, new Date().toISOString()) ?? 0)} ·{' '}
              {r.trigger === 'manual' ? `by ${r.requested_by}` : r.trigger}
            </div>
          </div>
        </li>
      ))}
    </ul>
  );
}

function FiringAlerts({ alerts, links }: { alerts: AlertEvent[]; links: OverviewLink[] }) {
  if (!alerts.length)
    return (
      <Empty>
        <span aria-hidden="true">✔</span> No alerts are firing.
      </Empty>
    );
  const names = new Map(links.map((l) => [l.agent.id, l.agent.name]));
  return (
    <ul className="plain-list">
      {alerts.map((a) => (
        <li key={a.id} className="alert-item">
          <Badge kind="warning" icon="▲" label="Firing" />
          <div>
            <strong>{a.rule_name || `Rule ${a.rule_id}`}</strong> · {names.get(a.agent_id) ?? a.agent_id}
            <div className="cell-sub">
              {a.message} · since {fmtTime(a.started_at)}
            </div>
          </div>
        </li>
      ))}
    </ul>
  );
}
