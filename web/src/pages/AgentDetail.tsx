import { useEffect, useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { agentState } from '../lib/domain';
import { fmtAgo, fmtTime, fmtUnix, toLocalInput } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { Link, navigate } from '../lib/router';
import type { Agent, Annotation, Run } from '../lib/types';
import { Button, Card, ConfirmButton, DataTable, Empty, ErrorBox, Field, KV, Loading, PageHeader, Segmented, StatusDot, toast } from '../components/ui';
import { RunsTable } from '../components/shared';
import { HostDiagnostics } from '../components/HostDiag';
import { Heatmap, MonitorCharts, RANGES, type RangeId } from '../components/Monitor';

export function AgentDetail({ id }: { id: string }) {
  const agent = useApi<Agent>(`/api/agents/${encodeURIComponent(id)}`);
  const runs = useApi<Run[]>(`/api/runs?agent=${encodeURIComponent(id)}&limit=20`);
  const ann = useApi<Annotation[]>(`/api/annotations/${encodeURIComponent(id)}`);
  const isOperator = useCan('operator');
  const isAdmin = useCan('admin');
  const [range, setRange] = useState<RangeId>('24h');
  useDocumentTitle(agent.data?.name ?? 'Agent');
  useLiveRefresh((m) => (m.type === 'agent_status' && m.agent_id === id) || (m.type === 'run_status' && (m.agent_id === id || !m.agent_id)), () => {
    agent.reload();
    runs.reload();
  });

  if (agent.error) {
    return (
      <div className="page">
        <PageHeader title="Agent" />
        <ErrorBox error={agent.error} onRetry={agent.reload} />
        <Link to="/agents">Back to agents</Link>
      </div>
    );
  }
  const a = agent.data;
  if (!a)
    return (
      <div className="page">
        <Loading />
      </div>
    );
  const st = agentState(a);
  const pol = a.policy;

  const revoke = async () => {
    try {
      await api.post(`/api/agents/${encodeURIComponent(a.id)}/revoke`);
      toast(`${a.name} revoked`);
      agent.reload();
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };
  const remove = async () => {
    try {
      await api.del(`/api/agents/${encodeURIComponent(a.id)}`);
      toast(`${a.name} deleted`);
      navigate('/agents');
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };

  return (
    <div className="page">
      <PageHeader
        title={a.name}
        sub={
          <span className="inline-list">
            <StatusDot kind={st.kind} label={st.label} icon={st.icon} />
            {a.site && <span>{a.site}</span>}
            <span className="muted">
              <code className="small">{a.id}</code>
            </span>
          </span>
        }
        actions={
          <>
            <Link className="btn" to={`/monitoring?agent=${a.id}`}>
              Monitoring
            </Link>
            {isOperator &&
              !a.revoked &&
              (a.online && !a.busy ? (
                <Link className="btn btn-primary" to={`/runs/new?agent=${a.id}`}>
                  New run
                </Link>
              ) : (
                <Button variant="primary" disabled title={a.busy ? 'The agent is running a test' : 'The agent is offline'}>
                  New run
                </Button>
              ))}
          </>
        }
      />
      <div className="grid-2">
        <Card title="Status">
          <KV
            rows={[
              ['Connection', <StatusDot kind={st.kind} label={st.label} icon={st.icon} />],
              ['Last seen', a.online ? 'connected now' : `${fmtAgo(a.last_seen)} (${fmtTime(a.last_seen)})`],
              ['Version', a.version || '—'],
              ['Observed address', <code>{a.observed_addr || '—'}</code>],
              ['Enrolled', fmtTime(a.enrolled_at)],
              ['Local policy', pol ? `max ${pol.max_rate_mbps ?? '?'} Mbit/s · shaping ${pol.shaping || 'none'}${pol.allowed_tests?.length ? ` · tests: ${pol.allowed_tests.join(', ')}` : ''}${pol.allowed_hours?.length ? ` · hours: ${pol.allowed_hours.join(', ')}` : ''}` : '—'],
              ['Monitoring', a.monitor_enabled ? `on, every ${a.monitor_interval} s` : 'off'],
            ]}
          />
          {isAdmin && (
            <div className="danger-zone">
              <h3 className="subhead">Administration</h3>
              <p className="muted small">Revoking disconnects the agent and rejects its certificate. Deleting also removes its monitoring data and schedules.</p>
              <div className="row-actions">
                {!a.revoked && <ConfirmButton label="Revoke agent" confirmLabel="Revoke" prompt={`Revoke ${a.name}?`} onConfirm={revoke} small={false} />}
                <ConfirmButton label="Delete agent" confirmLabel="Delete" prompt={`Delete ${a.name} and its data?`} onConfirm={remove} small={false} />
              </div>
            </div>
          )}
        </Card>
        <Card title="Settings">
          <AgentSettings a={a} editable={isOperator && !a.revoked} onSaved={(x) => agent.setData({ ...a, ...x, host: a.host })} />
        </Card>
      </div>

      <Card
        title="Monitoring"
        actions={<Segmented label="Time range" options={RANGES.map((r) => ({ id: r.id, label: r.label }))} value={range} onChange={setRange} />}
      >
        <MonitorCharts agentId={a.id} hours={RANGES.find((r) => r.id === range)?.hours ?? 24} annotations={ann.data} />
        <h3 className="subhead">Latency heatmap</h3>
        <Heatmap agentId={a.id} />
      </Card>

      <Card title="Annotations">
        <Annotations agentId={a.id} list={ann.data} error={ann.error} reload={ann.reload} canEdit={isOperator} />
      </Card>

      <Card title="Recent runs" actions={<Link to={`/reports?agent=${a.id}`}>All runs</Link>}>
        <ErrorBox error={runs.error} onRetry={runs.reload} />
        {runs.data ? <RunsTable runs={runs.data} showAgent={false} empty="This agent has not run any tests yet." /> : !runs.error && <Loading />}
      </Card>

      <Card title="Host diagnostics">
        <HostDiagnostics host={a.host} />
      </Card>
    </div>
  );
}

function AgentSettings({ a, editable, onSaved }: { a: Agent; editable: boolean; onSaved: (a: Agent) => void }) {
  const [f, setF] = useState(() => formOf(a));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => setF(formOf(a)), [a]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    const interval = Number(f.monitor_interval);
    if (!f.name.trim()) return setError('The name cannot be empty.');
    if (f.monitor_enabled && (!Number.isFinite(interval) || interval < 10)) return setError('The monitoring interval must be at least 10 seconds.');
    setBusy(true);
    try {
      const saved = await api.put<Agent>(`/api/agents/${encodeURIComponent(a.id)}`, {
        name: f.name.trim(),
        site: f.site.trim(),
        expected_down: Number(f.expected_down) || 0,
        expected_up: Number(f.expected_up) || 0,
        monitor_enabled: f.monitor_enabled,
        monitor_interval: interval >= 10 ? interval : a.monitor_interval,
        notes: f.notes,
      });
      onSaved(saved);
      toast('Settings saved');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="form-grid" onSubmit={save}>
      <fieldset disabled={!editable} className="plain-fieldset">
        <Field label="Name">
          <input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} maxLength={64} required />
        </Field>
        <Field label="Site">
          <input value={f.site} onChange={(e) => setF({ ...f, site: e.target.value })} placeholder="e.g. London office" />
        </Field>
        <div className="form-row">
          <Field label="Expected download (Mbit/s)" hint="Contracted rate; 0 = unknown">
            <input type="number" min={0} step="any" value={f.expected_down} onChange={(e) => setF({ ...f, expected_down: e.target.value })} />
          </Field>
          <Field label="Expected upload (Mbit/s)">
            <input type="number" min={0} step="any" value={f.expected_up} onChange={(e) => setF({ ...f, expected_up: e.target.value })} />
          </Field>
        </div>
        <div className="form-row">
          <label className="check">
            <input type="checkbox" checked={f.monitor_enabled} onChange={(e) => setF({ ...f, monitor_enabled: e.target.checked })} />
            Continuous monitoring
          </label>
          <Field label="Interval (seconds)" hint="Minimum 10">
            <input type="number" min={10} value={f.monitor_interval} onChange={(e) => setF({ ...f, monitor_interval: e.target.value })} disabled={!f.monitor_enabled} />
          </Field>
        </div>
        <Field label="Notes" wide>
          <textarea rows={2} value={f.notes} onChange={(e) => setF({ ...f, notes: e.target.value })} />
        </Field>
      </fieldset>
      <ErrorBox error={error} />
      {editable ? (
        <div className="form-actions">
          <Button type="submit" variant="primary" busy={busy}>
            Save settings
          </Button>
        </div>
      ) : (
        <p className="muted small">You need the operator role to change these settings.</p>
      )}
    </form>
  );
}

function formOf(a: Agent) {
  return {
    name: a.name,
    site: a.site,
    expected_down: String(a.expected_down || ''),
    expected_up: String(a.expected_up || ''),
    monitor_enabled: a.monitor_enabled,
    monitor_interval: String(a.monitor_interval || 60),
    notes: a.notes,
  };
}

function Annotations({
  agentId,
  list,
  error,
  reload,
  canEdit,
}: {
  agentId: string;
  list: Annotation[] | undefined;
  error: string | null;
  reload: () => void;
  canEdit: boolean;
}) {
  const now = new Date();
  const [start, setStart] = useState(toLocalInput(new Date(now.getTime() - 3600 * 1000)));
  const [end, setEnd] = useState(toLocalInput(now));
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null);
    const s = new Date(start).getTime();
    const en = new Date(end).getTime();
    if (!text.trim()) return setErr('Enter the annotation text.');
    if (!Number.isFinite(s)) return setErr('Enter a start time.');
    setBusy(true);
    try {
      await api.post('/api/annotations', {
        agent_id: agentId,
        ts_start: Math.floor(s / 1000),
        ts_end: Math.floor((Number.isFinite(en) ? en : s) / 1000),
        text: text.trim(),
      });
      setText('');
      toast('Annotation added');
      reload();
    } catch (e2) {
      setErr(errorMessage(e2));
    } finally {
      setBusy(false);
    }
  };

  const del = async (id: number) => {
    try {
      await api.del(`/api/annotations/${id}`);
      reload();
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };

  return (
    <>
      <p className="muted small">Annotations mark changes or incidents (for example “ISP maintenance” or “SQM enabled”) and appear as shaded areas on the monitoring charts.</p>
      <ErrorBox error={error} onRetry={reload} />
      {list && list.length === 0 && <Empty>No annotations.</Empty>}
      {list && list.length > 0 && (
        <DataTable
          head={['From', 'To', 'Text', 'Author', '']}
          rows={[...list]
            .sort((x, y) => y.ts_start - x.ts_start)
            .map((n) => [
              fmtUnix(n.ts_start),
              n.ts_end > n.ts_start ? fmtUnix(n.ts_end) : <span className="muted">—</span>,
              n.text,
              n.author || '—',
              canEdit ? <ConfirmButton label="Delete" onConfirm={() => del(n.id)} /> : null,
            ])}
        />
      )}
      {canEdit && (
        <form className="form-row annotation-form" onSubmit={add}>
          <Field label="From">
            <input type="datetime-local" value={start} onChange={(e) => setStart(e.target.value)} required />
          </Field>
          <Field label="To">
            <input type="datetime-local" value={end} onChange={(e) => setEnd(e.target.value)} />
          </Field>
          <Field label="Text" wide>
            <input value={text} onChange={(e) => setText(e.target.value)} placeholder="What happened?" maxLength={200} />
          </Field>
          <Button type="submit" busy={busy}>
            Add annotation
          </Button>
        </form>
      )}
      <ErrorBox error={err} />
    </>
  );
}
