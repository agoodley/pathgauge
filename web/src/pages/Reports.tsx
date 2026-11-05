import { useRef, useState } from 'react';
import { api, errorMessage, reportURL } from '../lib/api';
import { useCan } from '../lib/auth';
import { fmtMbps, fmtTime, isNum } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { runLabel } from '../lib/domain';
import { Link, navigate, useQuery } from '../lib/router';
import type { Agent, Run } from '../lib/types';
import { Button, Card, Empty, ErrorBox, Field, Loading, Notice, PageHeader, ScoreBadge, Segmented, toast } from '../components/ui';
import { AgentSelect, RunStatusBadge, RunsTable, profileLabel } from '../components/shared';

export function Reports() {
  useDocumentTitle('Runs & reports');
  const [q, setQ] = useQuery();
  const agentFilter = q.get('agent') ?? '';
  const view = q.get('view') === 'all' ? 'all' : 'done';
  const agents = useApi<Agent[]>('/api/agents');
  const runs = useApi<Run[]>(`/api/runs?limit=200${view === 'done' ? '&status=done' : ''}${agentFilter ? `&agent=${encodeURIComponent(agentFilter)}` : ''}`);
  const canImport = useCan('operator');
  useLiveRefresh((m) => m.type === 'run_status', runs.reload);

  return (
    <div className="page">
      <PageHeader title="Runs & reports" sub="Completed runs with their HTML reports (technical for engineers, executive for management)." />
      <div className="toolbar">
        <Field label="Agent">
          <AgentSelect agents={agents.data ?? []} value={agentFilter} onChange={(v) => setQ({ agent: v })} allowAll="All agents" />
        </Field>
        <div className="field">
          <span className="field-label">Show</span>
          <Segmented
            label="Which runs"
            options={[
              { id: 'done', label: 'Completed' },
              { id: 'all', label: 'All runs' },
            ]}
            value={view}
            onChange={(v) => setQ({ view: v === 'all' ? 'all' : null })}
          />
        </div>
      </div>
      <Card title={view === 'done' ? 'Reports' : 'All runs'}>
        <ErrorBox error={runs.error} onRetry={runs.reload} />
        {!runs.data && !runs.error && <Loading />}
        {runs.data && view === 'all' && <RunsTable runs={runs.data} />}
        {runs.data && view === 'done' && <ReportTable runs={runs.data} />}
      </Card>
      {canImport && <ImportCard agents={agents.data ?? []} />}
    </div>
  );
}

function ReportTable({ runs }: { runs: Run[] }) {
  if (!runs.length) return <Empty>No completed runs yet.</Empty>;
  return (
    <div className="table-wrap">
      <table className="data">
        <thead>
          <tr>
            <th scope="col">Run</th>
            <th scope="col">Agent</th>
            <th scope="col">Profile</th>
            <th scope="col">Health</th>
            <th scope="col" className="num">
              Down / up
            </th>
            <th scope="col">Technical report</th>
            <th scope="col">Executive report</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id}>
              <td>
                <Link to={`/runs/${r.id}`}>{fmtTime(r.started_at || r.created_at)}</Link>
                <div className="cell-sub">
                  <RunStatusBadge run={r} /> {r.trigger}
                </div>
              </td>
              <td>{r.agent_name ? <Link to={`/agents/${r.agent_id}`}>{r.agent_name}</Link> : runLabel(r)}</td>
              <td>{profileLabel(r)}</td>
              <td>
                <ScoreBadge score={r.health_score} />
              </td>
              <td className="num">
                {isNum(r.down_mbps) && r.down_mbps > 0 ? fmtMbps(r.down_mbps) : '—'} / {isNum(r.up_mbps) && r.up_mbps > 0 ? fmtMbps(r.up_mbps) : '—'}
              </td>
              <td>
                <span className="row-actions">
                  <a href={reportURL(r.id, 'technical')} target="_blank" rel="noopener">
                    Open ↗
                  </a>
                  <a href={reportURL(r.id, 'technical', true)} download>
                    Download
                  </a>
                </span>
              </td>
              <td>
                <span className="row-actions">
                  <a href={reportURL(r.id, 'executive')} target="_blank" rel="noopener">
                    Open ↗
                  </a>
                  <a href={reportURL(r.id, 'executive', true)} download>
                    Download
                  </a>
                </span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ImportCard({ agents }: { agents: Agent[] }) {
  const fileRef = useRef<HTMLInputElement>(null);
  const [agent, setAgent] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState('');

  const upload = async () => {
    setError(null);
    const f = fileRef.current?.files?.[0];
    if (!f) return setError('Choose a results file first.');
    if (f.size > 32 * 1024 * 1024) return setError('The file is larger than 32 MB.');
    setBusy(true);
    try {
      const text = await f.text();
      try {
        const j = JSON.parse(text) as { schema?: number };
        if (!j || typeof j !== 'object' || !j.schema) throw new Error('x');
      } catch {
        throw new Error('This is not a PathGauge results file (JSON with a "schema" field).');
      }
      const r = await api.postRaw<{ run_id: string }>(`/api/import${agent ? `?agent=${encodeURIComponent(agent)}` : ''}`, text);
      toast('Results imported');
      navigate(`/runs/${r.run_id}`);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title="Import results">
      <p className="muted">
        Import a JSON results file saved by <code>pathgauge test</code> (written with <code>--json FILE</code> or saved under <code>~/.local/share/pathgauge/results</code>). The findings are recomputed on import.
      </p>
      <div className="form-row">
        <Field label="Results file (.json)">
          <input
            ref={fileRef}
            type="file"
            accept="application/json,.json"
            onChange={(e) => {
              setName(e.target.files?.[0]?.name ?? '');
              setError(null);
            }}
          />
        </Field>
        <Field label="Attach to agent (optional)">
          <AgentSelect agents={agents} value={agent} onChange={setAgent} allowAll="None (imported)" />
        </Field>
        <Button variant="primary" onClick={() => void upload()} busy={busy} disabled={!name}>
          Import
        </Button>
      </div>
      <ErrorBox error={error} />
      {!agents.length && <Notice kind="info">Imported runs without an agent are listed as “imported”.</Notice>}
    </Card>
  );
}
