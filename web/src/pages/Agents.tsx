import { useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { agentState } from '../lib/domain';
import { fmtAgo, fmtTime, parseTime } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { Link } from '../lib/router';
import type { Agent, EnrollToken } from '../lib/types';
import { Badge, Button, Card, CodeLine, ConfirmButton, DataTable, Empty, ErrorBox, Field, Loading, Notice, PageHeader, StatusDot, toast } from '../components/ui';
import { LastSeen } from '../components/shared';

export function AgentsPage() {
  useDocumentTitle('Agents');
  const agents = useApi<Agent[]>('/api/agents');
  const isAdmin = useCan('admin');
  const isOperator = useCan('operator');
  useLiveRefresh((m) => m.type === 'agent_status' || m.type === 'run_status', agents.reload);

  return (
    <div className="page">
      <PageHeader title="Agents" sub="Enrolled test agents. Each agent tests the path between its host and this server." />
      <ErrorBox error={agents.error} onRetry={agents.reload} />
      <Card title="Agents">
        {!agents.data && !agents.error && <Loading />}
        {agents.data && agents.data.length === 0 && <Empty>No agents yet.{isAdmin ? ' Create a join token below to enrol one.' : ''}</Empty>}
        {agents.data && agents.data.length > 0 && (
          <DataTable
            head={['Agent', 'Status', 'Site', 'Version', 'Observed address', 'Last seen', 'Monitoring', '']}
            rows={agents.data.map((a) => {
              const st = agentState(a);
              return [
                <Link to={`/agents/${a.id}`}>
                  <strong>{a.name}</strong>
                </Link>,
                <StatusDot kind={st.kind} label={st.label} icon={st.icon} />,
                a.site || <span className="muted">—</span>,
                a.version || '—',
                <code className="small">{a.observed_addr || '—'}</code>,
                <LastSeen a={a} />,
                a.monitor_enabled ? `every ${a.monitor_interval} s` : <span className="muted">off</span>,
                <span className="row-actions">
                  <Link to={`/monitoring?agent=${a.id}`}>Monitoring</Link>
                  {isOperator && a.online && !a.busy && !a.revoked && <Link to={`/runs/new?agent=${a.id}`}>New run</Link>}
                </span>,
              ];
            })}
          />
        )}
      </Card>
      {isAdmin && <Enrollment />}
    </div>
  );
}

function Enrollment() {
  const tokens = useApi<EnrollToken[] | null>('/api/enroll-tokens');
  const [hours, setHours] = useState('24');
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<{ token: string; join_command: string; expires_in_hours: number } | null>(null);
  const [error, setError] = useState<string | null>(null);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const r = await api.post<{ token: string; join_command: string; expires_in_hours: number }>('/api/enroll-tokens', {
        hours: Number(hours) || 24,
        note: note.trim(),
      });
      setCreated(r);
      setNote('');
      tokens.reload();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const del = async (id: number) => {
    try {
      await api.del(`/api/enroll-tokens/${id}`);
      toast('Join token deleted');
      tokens.reload();
    } catch (err) {
      toast(errorMessage(err), 'critical');
    }
  };

  const now = Date.now();
  // The endpoint returns null (not []) when there are no tokens.
  const list = tokens.data === null ? [] : tokens.data;
  return (
    <Card title="Add an agent">
      <p className="muted">
        Create a single-use join token, then run the join command on the agent host. The token is shown only once.
      </p>
      <form className="form-row" onSubmit={create}>
        <Field label="Valid for (hours)">
          <input type="number" min={1} max={720} value={hours} onChange={(e) => setHours(e.target.value)} required />
        </Field>
        <Field label="Note (optional)" wide>
          <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="e.g. Branch office router" maxLength={200} />
        </Field>
        <Button type="submit" variant="primary" busy={busy}>
          Create join token
        </Button>
      </form>
      <ErrorBox error={error} />
      {created && (
        <Notice kind="good">
          <p>
            <strong>Join token created.</strong> It expires in {created.expires_in_hours} h and can be used once. Copy it now: it will not be
            shown again.
          </p>
          <div className="field-label">Token</div>
          <CodeLine text={created.token} />
          <div className="field-label">Join command (replace &lt;agent-name&gt;)</div>
          <CodeLine text={created.join_command} />
        </Notice>
      )}
      <h3 className="subhead">Join tokens</h3>
      <ErrorBox error={tokens.error} onRetry={tokens.reload} />
      {list && list.length === 0 && <Empty>No join tokens.</Empty>}
      {list && list.length > 0 && (
        <DataTable
          head={['Created', 'By', 'Note', 'Expires', 'State', '']}
          rows={list.map((t) => {
            const expired = (parseTime(t.expires_at)?.getTime() ?? 0) < now;
            const state = t.used_at ? (
              <Badge kind="neutral" icon="✔" label={`Used ${fmtAgo(t.used_at)}`} title={t.used_by} />
            ) : expired ? (
              <Badge kind="neutral" icon="⊘" label="Expired" />
            ) : (
              <Badge kind="info" icon="◌" label="Unused" />
            );
            return [
              fmtTime(t.created_at),
              t.created_by || '—',
              t.note || <span className="muted">—</span>,
              fmtTime(t.expires_at),
              <span>
                {state}
                {t.used_by && (
                  <>
                    {' '}
                    <Link to={`/agents/${t.used_by}`} className="small">
                      agent
                    </Link>
                  </>
                )}
              </span>,
              <ConfirmButton label="Delete" onConfirm={() => del(t.id)} />,
            ];
          })}
        />
      )}
    </Card>
  );
}
