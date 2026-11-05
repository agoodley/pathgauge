import { useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { fmtAgo, fmtTime } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { Link } from '../lib/router';
import type { Agent, Profiles, Schedule } from '../lib/types';
import { Badge, Button, Card, ConfirmButton, DataTable, Empty, ErrorBox, Field, Loading, Modal, PageHeader, Segmented, toast } from '../components/ui';
import { AgentSelect, useProfiles } from '../components/shared';

const ORDER = ['quick', 'standard', 'full'];

const PRESETS = [
  { cron: '0 3 * * *', label: 'Daily at 03:00' },
  { cron: '0 */6 * * *', label: 'Every 6 hours' },
  { cron: '0 * * * *', label: 'Hourly' },
  { cron: '0 8 * * 1-5', label: 'Weekdays at 08:00' },
  { cron: '0 2 * * 0', label: 'Sundays at 02:00' },
];

/** Plain-English reading of common 5-field cron expressions. */
export function describeCron(c: string): string {
  const p = c.trim().split(/\s+/);
  if (p.length !== 5) return '';
  const [min, hour, dom, mon, dow] = p;
  const hhmm = /^\d+$/.test(min) && /^\d+$/.test(hour) ? `${hour.padStart(2, '0')}:${min.padStart(2, '0')}` : '';
  if (dom === '*' && mon === '*') {
    if (hhmm && dow === '*') return `every day at ${hhmm}`;
    if (hhmm && dow === '1-5') return `weekdays at ${hhmm}`;
    const days = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
    if (hhmm && /^[0-6]$/.test(dow)) return `${days[Number(dow)]}s at ${hhmm}`;
    if (/^\d+$/.test(min) && hour === '*' && dow === '*') return `every hour at :${min.padStart(2, '0')}`;
    const m = /^\*\/(\d+)$/.exec(hour);
    if (/^\d+$/.test(min) && m && dow === '*') return `every ${m[1]} hours at :${min.padStart(2, '0')}`;
    const mm = /^\*\/(\d+)$/.exec(min);
    if (mm && hour === '*' && dow === '*') return `every ${mm[1]} minutes`;
  }
  return '';
}

function what(s: Schedule, p: Profiles | undefined): string {
  if (s.tests) {
    const t = s.tests.split(',').filter(Boolean);
    return `custom: ${t.length <= 3 ? t.map((x) => p?.titles[x] ?? x).join(', ') : `${t.length} tests`}`;
  }
  return `${s.profile || 'standard'} profile`;
}

export function Schedules() {
  useDocumentTitle('Schedules');
  const list = useApi<Schedule[]>('/api/schedules');
  const agents = useApi<Agent[]>('/api/agents');
  const profiles = useProfiles();
  const canEdit = useCan('operator');
  const [editing, setEditing] = useState<Schedule | null>(null);

  const names = new Map((agents.data ?? []).map((a) => [a.id, a.name]));

  const save = async (s: Schedule) => {
    if (s.id) await api.put(`/api/schedules/${s.id}`, s);
    else await api.post('/api/schedules', s);
    list.reload();
  };

  const toggle = async (s: Schedule) => {
    try {
      await save({ ...s, enabled: !s.enabled });
      toast(s.enabled ? 'Schedule paused' : 'Schedule enabled');
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };

  const del = async (s: Schedule) => {
    try {
      await api.del(`/api/schedules/${s.id}`);
      toast('Schedule deleted');
      list.reload();
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };

  const blank: Schedule = { id: 0, agent_id: '', profile: 'quick', tests: '', cron: '0 3 * * *', enabled: true, note: '' };

  return (
    <div className="page">
      <PageHeader
        title="Schedules"
        sub="Run test profiles automatically on a cron schedule (server time zone)."
        actions={
          canEdit && (
            <Button variant="primary" onClick={() => setEditing(blank)} disabled={!agents.data?.length}>
              New schedule
            </Button>
          )
        }
      />
      <Card>
        <ErrorBox error={list.error} onRetry={list.reload} />
        {!list.data && !list.error && <Loading />}
        {list.data && list.data.length === 0 && <Empty>No schedules yet.</Empty>}
        {list.data && list.data.length > 0 && (
          <DataTable
            head={['Agent', 'Tests', 'Schedule', 'State', 'Note', 'Last run', '']}
            rows={list.data.map((s) => [
              names.has(s.agent_id) ? <Link to={`/agents/${s.agent_id}`}>{names.get(s.agent_id)}</Link> : <code className="small">{s.agent_id}</code>,
              what(s, profiles),
              <>
                <code>{s.cron}</code>
                {describeCron(s.cron) && <div className="cell-sub">{describeCron(s.cron)}</div>}
              </>,
              s.enabled ? <Badge kind="good" icon="✔" label="Enabled" /> : <Badge kind="neutral" icon="⏸" label="Paused" />,
              s.note || <span className="muted">—</span>,
              s.last_run ? <span title={fmtTime(s.last_run)}>{fmtAgo(s.last_run)}</span> : <span className="muted">never</span>,
              canEdit ? (
                <span className="row-actions">
                  <Button small onClick={() => setEditing(s)}>
                    Edit
                  </Button>
                  <Button small variant="ghost" onClick={() => void toggle(s)}>
                    {s.enabled ? 'Pause' : 'Enable'}
                  </Button>
                  <ConfirmButton label="Delete" onConfirm={() => del(s)} />
                </span>
              ) : null,
            ])}
          />
        )}
      </Card>
      {editing && agents.data && (
        <ScheduleForm
          initial={editing}
          agents={agents.data}
          profiles={profiles}
          onClose={() => setEditing(null)}
          onSave={async (s) => {
            await save(s);
            toast(s.id ? 'Schedule saved' : 'Schedule created');
            setEditing(null);
          }}
        />
      )}
    </div>
  );
}

function ScheduleForm({
  initial,
  agents,
  profiles,
  onClose,
  onSave,
}: {
  initial: Schedule;
  agents: Agent[];
  profiles: Profiles | undefined;
  onClose: () => void;
  onSave: (s: Schedule) => Promise<void>;
}) {
  const [s, setS] = useState<Schedule>({ ...initial, agent_id: initial.agent_id || agents.find((a) => !a.revoked)?.id || '' });
  const [mode, setMode] = useState<'profile' | 'custom'>(initial.tests ? 'custom' : 'profile');
  const [tests, setTests] = useState<Set<string>>(new Set(initial.tests ? initial.tests.split(',') : ['info', 'latency', 'tcp-down', 'tcp-up']));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!s.agent_id) return setError('Choose an agent.');
    if (s.cron.trim().split(/\s+/).length !== 5) return setError('The cron expression needs 5 fields: minute hour day-of-month month day-of-week.');
    const list = (profiles?.tests ?? [...tests]).filter((t) => tests.has(t));
    if (mode === 'custom' && !list.length) return setError('Choose at least one test.');
    setBusy(true);
    try {
      await onSave({ ...s, cron: s.cron.trim(), tests: mode === 'custom' ? list.join(',') : '', profile: mode === 'custom' ? 'custom' : s.profile === 'custom' || !s.profile ? 'quick' : s.profile });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const desc = describeCron(s.cron);
  return (
    <Modal title={initial.id ? 'Edit schedule' : 'New schedule'} open onClose={onClose} wide>
      <form className="form-grid" onSubmit={submit}>
        <Field label="Agent">
          <AgentSelect agents={agents} value={s.agent_id} onChange={(v) => setS({ ...s, agent_id: v })} />
        </Field>
        <div className="field">
          <span className="field-label">Tests</span>
          <Segmented label="Test selection" options={[{ id: 'profile', label: 'Profile' }, { id: 'custom', label: 'Custom tests' }]} value={mode} onChange={setMode} />
        </div>
        {mode === 'profile' ? (
          <Field label="Profile">
            <select value={s.profile === 'custom' ? 'quick' : s.profile} onChange={(e) => setS({ ...s, profile: e.target.value })}>
              {Object.keys(profiles?.profiles ?? { quick: [], standard: [], full: [] })
                .sort((a, b) => ORDER.indexOf(a) - ORDER.indexOf(b))
                .map((p) => (
                <option key={p} value={p}>
                  {p} ({profiles?.profiles[p]?.length ?? '?'} tests)
                </option>
              ))}
            </select>
          </Field>
        ) : (
          <fieldset className="test-group test-group-compact">
            <legend>Custom tests</legend>
            {(profiles?.tests ?? []).map((t) => (
              <label key={t} className="check">
                <input
                  type="checkbox"
                  checked={tests.has(t)}
                  onChange={() => {
                    const n = new Set(tests);
                    if (n.has(t)) n.delete(t);
                    else n.add(t);
                    setTests(n);
                  }}
                />
                {profiles?.titles[t] ?? t}
              </label>
            ))}
          </fieldset>
        )}
        <Field label="Cron expression (5 fields)" hint={desc ? `Runs ${desc}.` : 'minute hour day-of-month month day-of-week, e.g. 0 3 * * *'}>
          <input value={s.cron} onChange={(e) => setS({ ...s, cron: e.target.value })} className="mono" required spellCheck={false} />
        </Field>
        <div className="chips" aria-label="Common schedules">
          {PRESETS.map((p) => (
            <button type="button" key={p.cron} className={`chip${s.cron === p.cron ? ' on' : ''}`} onClick={() => setS({ ...s, cron: p.cron })}>
              {p.label}
            </button>
          ))}
        </div>
        <Field label="Note">
          <input value={s.note} onChange={(e) => setS({ ...s, note: e.target.value })} placeholder="e.g. nightly quick check" />
        </Field>
        <label className="check">
          <input type="checkbox" checked={s.enabled} onChange={(e) => setS({ ...s, enabled: e.target.checked })} />
          Enabled
        </label>
        <ErrorBox error={error} />
        <div className="form-actions">
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" busy={busy}>
            {initial.id ? 'Save schedule' : 'Create schedule'}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
