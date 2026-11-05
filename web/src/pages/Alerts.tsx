import { useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { metricInfo } from '../lib/domain';
import { fmtTime, f2 } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { Link } from '../lib/router';
import type { Agent, AlertChannel, AlertEvent, AlertMetric, AlertRule } from '../lib/types';
import { Badge, Button, Card, ConfirmButton, DataTable, Empty, ErrorBox, Field, Loading, Modal, PageHeader, Tabs, toast } from '../components/ui';
import { AgentSelect } from '../components/shared';

const METRICS: AlertMetric[] = ['rtt_p95', 'loss_pct', 'jitter', 'offline', 'health_score', 'down_mbps', 'up_mbps'];
const CHANNELS: { id: AlertChannel; label: string; placeholder: string }[] = [
  { id: 'webhook', label: 'Webhook (JSON POST)', placeholder: 'https://example.com/hooks/pathgauge' },
  { id: 'slack', label: 'Slack incoming webhook', placeholder: 'https://hooks.slack.com/services/…' },
  { id: 'ntfy', label: 'ntfy topic', placeholder: 'https://ntfy.sh/my-topic' },
  { id: 'email', label: 'Email', placeholder: 'noc@example.com' },
  { id: 'log', label: 'Server log only', placeholder: '' },
];

function condition(r: AlertRule): string {
  const m = metricInfo[r.metric];
  const base = `${m?.label ?? r.metric} ${r.op} ${r.threshold} ${m?.unit ?? ''}`.trim();
  const runBased = r.metric === 'health_score' || r.metric === 'down_mbps' || r.metric === 'up_mbps';
  if (runBased) return `${base} (on each completed run)`;
  return r.for_minutes > 0 ? `${base} for ${r.for_minutes} min` : base;
}

export function Alerts() {
  useDocumentTitle('Alerts');
  const rules = useApi<AlertRule[]>('/api/alert-rules');
  const events = useApi<AlertEvent[]>('/api/alerts');
  const agents = useApi<Agent[]>('/api/agents');
  const canEdit = useCan('operator');
  const [tabChoice, setTab] = useState<'events' | 'rules' | null>(null);
  // Until the user picks a tab, open the rules when there are no events.
  const tab = tabChoice ?? (events.data && events.data.length === 0 ? 'rules' : 'events');
  const [editing, setEditing] = useState<AlertRule | null>(null);
  useLiveRefresh((m) => m.type === 'alert', events.reload);

  const names = new Map((agents.data ?? []).map((a) => [a.id, a.name]));
  const firing = (events.data ?? []).filter((e) => e.state === 'firing').length;

  const save = async (r: AlertRule) => {
    if (r.id) await api.put(`/api/alert-rules/${r.id}`, r);
    else await api.post('/api/alert-rules', r);
    rules.reload();
  };
  const test = async (r: AlertRule) => {
    try {
      await api.post(`/api/alert-rules/${r.id}/test`);
      toast(`Test notification sent via ${r.channel}`);
    } catch (e) {
      toast(`Test failed: ${errorMessage(e)}`, 'critical');
    }
  };
  const del = async (r: AlertRule) => {
    try {
      await api.del(`/api/alert-rules/${r.id}`);
      toast('Rule deleted');
      rules.reload();
      events.reload();
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };
  const toggle = async (r: AlertRule) => {
    try {
      await save({ ...r, enabled: !r.enabled });
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };

  const blank: AlertRule = { id: 0, name: '', agent_id: '', metric: 'rtt_p95', op: '>', threshold: 50, for_minutes: 5, channel: 'log', target: '', enabled: true };

  return (
    <div className="page">
      <PageHeader
        title="Alerts"
        sub={firing ? `${firing} alert${firing === 1 ? '' : 's'} firing` : 'No alerts firing'}
        actions={
          canEdit && (
            <Button
              variant="primary"
              onClick={() => {
                setTab('rules');
                setEditing(blank);
              }}
            >
              New rule
            </Button>
          )
        }
      />
      <Tabs
        tabs={[
          { id: 'events', label: `Events${firing ? ` (${firing} firing)` : ''}` },
          { id: 'rules', label: `Rules${rules.data ? ` (${rules.data.length})` : ''}` },
        ]}
        value={tab}
        onChange={setTab}
      />
      {tab === 'events' && (
        <Card>
          <ErrorBox error={events.error} onRetry={events.reload} />
          {!events.data && !events.error && <Loading />}
          {events.data && events.data.length === 0 && <Empty>No alert events yet.</Empty>}
          {events.data && events.data.length > 0 && (
            <DataTable
              head={['State', 'Rule', 'Agent', 'Value', 'Message', 'Started', 'Resolved', 'Notification']}
              numeric={[3]}
              rows={events.data.map((e) => [
                e.state === 'firing' ? <Badge kind="warning" icon="▲" label="Firing" /> : <Badge kind="good" icon="✔" label="Resolved" />,
                e.rule_name || `#${e.rule_id}`,
                names.has(e.agent_id) ? <Link to={`/agents/${e.agent_id}`}>{names.get(e.agent_id)}</Link> : e.agent_id,
                f2(e.value),
                e.message,
                fmtTime(e.started_at),
                e.resolved_at ? fmtTime(e.resolved_at) : <span className="muted">—</span>,
                e.notify_error ? <Badge kind="critical" icon="✖" label="Failed" title={e.notify_error} /> : <span className="muted">sent</span>,
              ])}
            />
          )}
        </Card>
      )}
      {tab === 'rules' && (
        <Card>
          <ErrorBox error={rules.error} onRetry={rules.reload} />
          {!rules.data && !rules.error && <Loading />}
          {rules.data && rules.data.length === 0 && <Empty>No alert rules yet.</Empty>}
          {rules.data && rules.data.length > 0 && (
            <DataTable
              head={['Rule', 'Agent', 'Condition', 'Notify', 'State', '']}
              rows={rules.data.map((r) => [
                <strong>{r.name}</strong>,
                r.agent_id ? names.get(r.agent_id) ?? r.agent_id : 'All agents',
                condition(r),
                <>
                  {r.channel}
                  {r.target && <div className="cell-sub">{r.target}</div>}
                </>,
                r.enabled ? <Badge kind="good" icon="✔" label="Enabled" /> : <Badge kind="neutral" icon="⏸" label="Disabled" />,
                canEdit ? (
                  <span className="row-actions">
                    <Button small onClick={() => setEditing(r)}>
                      Edit
                    </Button>
                    <Button small variant="ghost" onClick={() => void test(r)}>
                      Send test
                    </Button>
                    <Button small variant="ghost" onClick={() => void toggle(r)}>
                      {r.enabled ? 'Disable' : 'Enable'}
                    </Button>
                    <ConfirmButton label="Delete" onConfirm={() => del(r)} />
                  </span>
                ) : null,
              ])}
            />
          )}
        </Card>
      )}
      {editing && (
        <RuleForm
          initial={editing}
          agents={agents.data ?? []}
          onClose={() => setEditing(null)}
          onSave={async (r) => {
            await save(r);
            toast(r.id ? 'Rule saved' : 'Rule created');
            setEditing(null);
          }}
        />
      )}
    </div>
  );
}

function RuleForm({ initial, agents, onClose, onSave }: { initial: AlertRule; agents: Agent[]; onClose: () => void; onSave: (r: AlertRule) => Promise<void> }) {
  const [r, setR] = useState<AlertRule>(initial);
  const [threshold, setThreshold] = useState(String(initial.threshold));
  const [forMin, setForMin] = useState(String(initial.for_minutes));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const ch = CHANNELS.find((c) => c.id === r.channel);
  const runBased = r.metric === 'health_score' || r.metric === 'down_mbps' || r.metric === 'up_mbps';

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    const th = Number(threshold);
    const fm = Number(forMin);
    if (!r.name.trim()) return setError('Give the rule a name.');
    if (!Number.isFinite(th)) return setError('The threshold must be a number.');
    if (!Number.isInteger(fm) || fm < 0 || fm > 1440) return setError('“For” must be 0–1440 minutes.');
    if ((r.channel === 'webhook' || r.channel === 'slack' || r.channel === 'ntfy') && !/^https?:\/\//.test(r.target)) return setError('The target must be an http(s) URL.');
    if (r.channel === 'email' && !r.target.includes('@')) return setError('The target must be an email address.');
    setBusy(true);
    try {
      await onSave({ ...r, name: r.name.trim(), threshold: th, for_minutes: fm, target: r.channel === 'log' ? '' : r.target.trim() });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={initial.id ? 'Edit alert rule' : 'New alert rule'} open onClose={onClose} wide>
      <form className="form-grid" onSubmit={submit}>
        <Field label="Name">
          <input value={r.name} onChange={(e) => setR({ ...r, name: e.target.value })} required placeholder="e.g. High latency" />
        </Field>
        <Field label="Agent">
          <AgentSelect agents={agents} value={r.agent_id} onChange={(v) => setR({ ...r, agent_id: v })} allowAll="All agents" />
        </Field>
        <div className="form-row">
          <Field label="Metric" hint={metricInfo[r.metric]?.desc}>
            <select value={r.metric} onChange={(e) => setR({ ...r, metric: e.target.value as AlertMetric })}>
              {METRICS.map((m) => (
                <option key={m} value={m}>
                  {metricInfo[m].label} ({m})
                </option>
              ))}
            </select>
          </Field>
          <Field label="Condition">
            <select value={r.op} onChange={(e) => setR({ ...r, op: e.target.value as '>' | '<' })}>
              <option value=">">is above (&gt;)</option>
              <option value="<">is below (&lt;)</option>
            </select>
          </Field>
          <Field label={`Threshold (${metricInfo[r.metric]?.unit ?? ''})`}>
            <input type="number" step="any" value={threshold} onChange={(e) => setThreshold(e.target.value)} required />
          </Field>
          <Field label="For (minutes)" hint={runBased ? 'Not used: checked when each run finishes.' : 'Every sample in this window must breach.'}>
            <input type="number" min={0} max={1440} value={forMin} onChange={(e) => setForMin(e.target.value)} disabled={runBased} />
          </Field>
        </div>
        <div className="form-row">
          <Field label="Channel">
            <select value={r.channel} onChange={(e) => setR({ ...r, channel: e.target.value as AlertChannel })}>
              {CHANNELS.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.label}
                </option>
              ))}
            </select>
          </Field>
          {r.channel !== 'log' && (
            <Field label={r.channel === 'email' ? 'Email address' : 'URL'} wide hint={r.channel === 'email' ? 'Needs SMTP settings (Admin → Settings).' : undefined}>
              <input value={r.target} onChange={(e) => setR({ ...r, target: e.target.value })} placeholder={ch?.placeholder} required />
            </Field>
          )}
        </div>
        <label className="check">
          <input type="checkbox" checked={r.enabled} onChange={(e) => setR({ ...r, enabled: e.target.checked })} />
          Enabled
        </label>
        <ErrorBox error={error} />
        <div className="form-actions">
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" busy={busy}>
            {initial.id ? 'Save rule' : 'Create rule'}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
