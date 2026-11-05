import { useEffect } from 'react';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { Link, useQuery } from '../lib/router';
import { agentState } from '../lib/domain';
import type { Agent, Annotation } from '../lib/types';
import { Card, Empty, ErrorBox, Field, Loading, PageHeader, Segmented, StatusDot } from '../components/ui';
import { AgentSelect } from '../components/shared';
import { Heatmap, MonitorCharts, RANGES, type RangeId } from '../components/Monitor';

export function Monitoring() {
  useDocumentTitle('Monitoring');
  const [q, setQ] = useQuery();
  const agents = useApi<Agent[]>('/api/agents');
  const agentId = q.get('agent') ?? '';
  const rangeQ = q.get('range');
  const range: RangeId = RANGES.some((r) => r.id === rangeQ) ? (rangeQ as RangeId) : '24h';
  const hours = RANGES.find((r) => r.id === range)?.hours ?? 24;
  const ann = useApi<Annotation[]>(agentId ? `/api/annotations/${encodeURIComponent(agentId)}` : null);

  // Default to the first agent with monitoring enabled.
  useEffect(() => {
    if (!agentId && agents.data?.length) {
      const a = agents.data.find((x) => x.monitor_enabled && !x.revoked) ?? agents.data.find((x) => !x.revoked);
      if (a) setQ({ agent: a.id });
    }
  }, [agents.data, agentId]);

  const agent = agents.data?.find((a) => a.id === agentId);
  const st = agent ? agentState(agent) : null;

  return (
    <div className="page">
      <PageHeader title="Monitoring" sub="Continuous latency, jitter and loss measured by each agent against this server." />
      <div className="toolbar">
        <Field label="Agent">
          {agents.data ? <AgentSelect agents={agents.data} value={agentId} onChange={(v) => setQ({ agent: v })} /> : <select disabled />}
        </Field>
        <div className="field">
          <span className="field-label">Range</span>
          <Segmented label="Time range" options={RANGES.map((r) => ({ id: r.id, label: r.label }))} value={range} onChange={(v) => setQ({ range: v })} />
        </div>
        {agent && st && (
          <div className="toolbar-info">
            <StatusDot kind={st.kind} label={st.label} icon={st.icon} />
            <span className="muted">{agent.monitor_enabled ? `probing every ${agent.monitor_interval} s` : 'monitoring is off'}</span>
            <Link to={`/agents/${agent.id}`}>Agent details</Link>
          </div>
        )}
      </div>
      <ErrorBox error={agents.error} onRetry={agents.reload} />
      {!agents.data && !agents.error && <Loading />}
      {agents.data && agents.data.length === 0 && <Empty>No agents are enrolled.</Empty>}
      {agentId && (
        <>
          <Card title={`Latency, jitter and loss · last ${RANGES.find((r) => r.id === range)?.label}`}>
            <MonitorCharts key={`${agentId}-${hours}`} agentId={agentId} hours={hours} annotations={ann.data} />
          </Card>
          <Card title="Hour × weekday heatmap">
            <Heatmap key={agentId} agentId={agentId} />
          </Card>
          {ann.data && ann.data.length > 0 && (
            <Card title="Annotations">
              <ul className="plain-list">
                {ann.data.map((a) => (
                  <li key={a.id}>
                    <strong>{a.text}</strong>{' '}
                    <span className="muted">
                      {new Date(a.ts_start * 1000).toLocaleString()}
                      {a.ts_end > a.ts_start ? ` – ${new Date(a.ts_end * 1000).toLocaleString()}` : ''} · {a.author}
                    </span>
                  </li>
                ))}
              </ul>
              <p className="muted small">
                Add or remove annotations on the <Link to={`/agents/${agentId}`}>agent page</Link>.
              </p>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
