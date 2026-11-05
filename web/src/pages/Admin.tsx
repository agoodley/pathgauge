import { useEffect, useMemo, useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useAuth, useCan } from '../lib/auth';
import { fmtAgo, fmtTime } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { navigate } from '../lib/router';
import type { APIToken, AuditEntry, Role, Settings, User } from '../lib/types';
import { Badge, Button, Card, CodeLine, ConfirmButton, DataTable, Empty, ErrorBox, Field, Loading, Modal, Notice, PageHeader, Tabs, toast } from '../components/ui';

type Tab = 'users' | 'tokens' | 'settings' | 'audit';
const TABS: { id: Tab; label: string }[] = [
  { id: 'users', label: 'Users' },
  { id: 'tokens', label: 'API tokens' },
  { id: 'settings', label: 'Settings' },
  { id: 'audit', label: 'Audit log' },
];
const ROLES: Role[] = ['viewer', 'operator', 'admin'];
const ROLE_TEXT: Record<Role, string> = {
  viewer: 'read-only',
  operator: 'runs tests, schedules, alerts',
  admin: 'everything, including users and agents',
};

export function Admin({ tab }: { tab: string }) {
  const isAdmin = useCan('admin');
  const t: Tab = TABS.some((x) => x.id === tab) ? (tab as Tab) : 'users';
  useDocumentTitle(`Admin · ${TABS.find((x) => x.id === t)?.label}`);
  if (!isAdmin)
    return (
      <div className="page">
        <PageHeader title="Administration" />
        <Notice kind="info">Administration needs the admin role.</Notice>
      </div>
    );
  return (
    <div className="page">
      <PageHeader title="Administration" />
      <Tabs tabs={TABS} value={t} onChange={(v) => navigate(`/admin/${v}`, { keepScroll: true })} />
      {t === 'users' && <Users />}
      {t === 'tokens' && <Tokens />}
      {t === 'settings' && <SettingsForm />}
      {t === 'audit' && <Audit />}
    </div>
  );
}

function Users() {
  const users = useApi<User[]>('/api/users');
  const { me } = useAuth();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<Role>('viewer');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pwFor, setPwFor] = useState<User | null>(null);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!username.trim()) return setError('Enter a username.');
    if (password.length < 10) return setError('The password must be at least 10 characters.');
    setBusy(true);
    try {
      await api.post('/api/users', { username: username.trim(), password, role });
      toast(`User ${username.trim()} created`);
      setUsername('');
      setPassword('');
      users.reload();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const update = async (u: User, patch: Partial<{ role: Role; disabled: boolean; password: string }>, msg: string) => {
    try {
      await api.put(`/api/users/${u.id}`, { role: patch.role ?? u.role, disabled: patch.disabled ?? u.disabled, password: patch.password ?? '' });
      toast(msg);
      users.reload();
    } catch (err) {
      toast(errorMessage(err), 'critical');
      users.reload();
    }
  };

  const del = async (u: User) => {
    try {
      await api.del(`/api/users/${u.id}`);
      toast(`User ${u.username} deleted`);
      users.reload();
    } catch (err) {
      toast(errorMessage(err), 'critical');
    }
  };

  return (
    <>
      <Card title="Users">
        <ErrorBox error={users.error} onRetry={users.reload} />
        {!users.data && !users.error && <Loading />}
        {users.data && (
          <DataTable
            head={['User', 'Role', 'Status', 'Created', '']}
            rows={users.data.map((u) => {
              const self = u.id === me.user_id;
              return [
                <>
                  <strong>{u.username}</strong>
                  {self && <span className="muted"> (you)</span>}
                </>,
                <select
                  aria-label={`Role of ${u.username}`}
                  value={u.role}
                  disabled={self}
                  onChange={(e) => void update(u, { role: e.target.value as Role }, `${u.username} is now ${e.target.value}`)}
                >
                  {ROLES.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>,
                u.disabled ? <Badge kind="neutral" icon="⊘" label="Disabled" /> : <Badge kind="good" icon="✔" label="Active" />,
                fmtTime(u.created_at),
                <span className="row-actions">
                  <Button small onClick={() => setPwFor(u)}>
                    Set password
                  </Button>
                  {!self && (
                    <Button small variant="ghost" onClick={() => void update(u, { disabled: !u.disabled }, u.disabled ? `${u.username} enabled` : `${u.username} disabled`)}>
                      {u.disabled ? 'Enable' : 'Disable'}
                    </Button>
                  )}
                  {!self && <ConfirmButton label="Delete" prompt={`Delete ${u.username}?`} onConfirm={() => del(u)} />}
                </span>,
              ];
            })}
          />
        )}
      </Card>
      <Card title="Add a user">
        <form className="form-row" onSubmit={create}>
          <Field label="Username">
            <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" required />
          </Field>
          <Field label="Password" hint="At least 10 characters">
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" required minLength={10} />
          </Field>
          <Field label="Role" hint={ROLE_TEXT[role]}>
            <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </Field>
          <Button type="submit" variant="primary" busy={busy}>
            Create user
          </Button>
        </form>
        <ErrorBox error={error} />
      </Card>
      {pwFor && <PasswordDialog user={pwFor} onClose={() => setPwFor(null)} onSave={(pw) => update(pwFor, { password: pw }, `Password changed for ${pwFor.username}`)} />}
    </>
  );
}

function PasswordDialog({ user, onClose, onSave }: { user: User; onClose: () => void; onSave: (pw: string) => Promise<void> }) {
  const [pw, setPw] = useState('');
  const [pw2, setPw2] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (pw.length < 10) return setError('The password must be at least 10 characters.');
    if (pw !== pw2) return setError('The passwords do not match.');
    setBusy(true);
    await onSave(pw);
    setBusy(false);
    onClose();
  };
  return (
    <Modal title={`Set password for ${user.username}`} open onClose={onClose}>
      <form className="form-grid" onSubmit={submit}>
        <p className="muted small">Changing the password signs the user out of every session.</p>
        <Field label="New password">
          <input type="password" value={pw} onChange={(e) => setPw(e.target.value)} autoComplete="new-password" autoFocus />
        </Field>
        <Field label="Confirm">
          <input type="password" value={pw2} onChange={(e) => setPw2(e.target.value)} autoComplete="new-password" />
        </Field>
        <ErrorBox error={error} />
        <div className="form-actions">
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" busy={busy}>
            Set password
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function Tokens() {
  const tokens = useApi<APIToken[]>('/api/tokens');
  const [name, setName] = useState('');
  const [role, setRole] = useState<Role>('viewer');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<{ name: string; token: string } | null>(null);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!name.trim()) return setError('Give the token a name.');
    setBusy(true);
    try {
      const r = await api.post<{ id: number; token: string }>('/api/tokens', { name: name.trim(), role });
      setCreated({ name: name.trim(), token: r.token });
      setName('');
      tokens.reload();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const del = async (t: APIToken) => {
    try {
      await api.del(`/api/tokens/${t.id}`);
      toast(`Token ${t.name} revoked`);
      tokens.reload();
    } catch (err) {
      toast(errorMessage(err), 'critical');
    }
  };

  return (
    <>
      <Card title="API tokens">
        <p className="muted">
          Tokens authenticate scripts and integrations (for example Prometheus scraping <code>/metrics</code>) with <code>Authorization: Bearer pgt_…</code>.
        </p>
        <ErrorBox error={tokens.error} onRetry={tokens.reload} />
        {!tokens.data && !tokens.error && <Loading />}
        {tokens.data && tokens.data.length === 0 && <Empty>No API tokens.</Empty>}
        {tokens.data && tokens.data.length > 0 && (
          <DataTable
            head={['Name', 'Role', 'Created by', 'Created', 'Last used', '']}
            rows={tokens.data.map((t) => [
              <strong>{t.name}</strong>,
              t.role,
              t.created_by || '—',
              fmtTime(t.created_at),
              t.last_used ? fmtAgo(t.last_used) : <span className="muted">never</span>,
              <ConfirmButton label="Revoke" confirmLabel="Revoke" onConfirm={() => del(t)} />,
            ])}
          />
        )}
      </Card>
      <Card title="Create a token">
        <form className="form-row" onSubmit={create}>
          <Field label="Name">
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. prometheus" required />
          </Field>
          <Field label="Role" hint={ROLE_TEXT[role]}>
            <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </Field>
          <Button type="submit" variant="primary" busy={busy}>
            Create token
          </Button>
        </form>
        <ErrorBox error={error} />
        {created && (
          <Notice kind="good">
            <p>
              <strong>Token “{created.name}” created.</strong> Copy it now: it is shown only once.
            </p>
            <CodeLine text={created.token} />
          </Notice>
        )}
      </Card>
    </>
  );
}

const SETTING_FIELDS: { key: string; label: string; hint?: string; type?: string; placeholder?: string }[] = [
  { key: 'server_name', label: 'Server name', hint: 'Shown as the endpoint name in reports. Empty = host name.' },
  { key: 'org_name', label: 'Organisation name', hint: 'Shown in report headers.' },
  { key: 'smtp_host', label: 'SMTP host', placeholder: 'smtp.example.com' },
  { key: 'smtp_port', label: 'SMTP port', placeholder: '587', type: 'number' },
  { key: 'smtp_user', label: 'SMTP user' },
  { key: 'smtp_password', label: 'SMTP password', type: 'password', hint: 'Leave unchanged to keep the stored password.' },
  { key: 'smtp_from', label: 'From address', placeholder: 'pathgauge@example.com' },
];

function SettingsForm() {
  const s = useApi<Settings>('/api/settings');
  const [form, setForm] = useState<Settings>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (s.data) setForm(s.data);
  }, [s.data]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const out = await api.put<Settings>('/api/settings', form);
      s.setData(out);
      toast('Settings saved');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title="Settings">
      <ErrorBox error={s.error} onRetry={s.reload} />
      {!s.data && !s.error && <Loading />}
      {s.data && (
        <form className="form-grid settings" onSubmit={save}>
          <h3 className="subhead">General</h3>
          {SETTING_FIELDS.slice(0, 2).map((f) => (
            <Field key={f.key} label={f.label} hint={f.hint}>
              <input value={form[f.key] ?? ''} onChange={(e) => setForm({ ...form, [f.key]: e.target.value })} placeholder={f.placeholder} />
            </Field>
          ))}
          <h3 className="subhead">Email (for email alerts)</h3>
          <div className="form-row wrap">
            {SETTING_FIELDS.slice(2).map((f) => (
              <Field key={f.key} label={f.label} hint={f.hint}>
                <input
                  type={f.type ?? 'text'}
                  value={form[f.key] ?? ''}
                  onChange={(e) => setForm({ ...form, [f.key]: e.target.value })}
                  placeholder={f.placeholder}
                  autoComplete={f.type === 'password' ? 'new-password' : 'off'}
                />
              </Field>
            ))}
          </div>
          <ErrorBox error={error} />
          <div className="form-actions">
            <Button type="submit" variant="primary" busy={busy}>
              Save settings
            </Button>
          </div>
        </form>
      )}
    </Card>
  );
}

function Audit() {
  const audit = useApi<AuditEntry[]>('/api/audit');
  const [filter, setFilter] = useState('');
  const rows = useMemo(() => {
    const f = filter.trim().toLowerCase();
    const list = audit.data ?? [];
    return f ? list.filter((e) => [e.actor, e.action, e.target, e.detail].some((x) => x?.toLowerCase().includes(f))) : list;
  }, [audit.data, filter]);
  return (
    <Card
      title="Audit log"
      actions={
        <input className="search" type="search" placeholder="Filter…" aria-label="Filter the audit log" value={filter} onChange={(e) => setFilter(e.target.value)} />
      }
    >
      <ErrorBox error={audit.error} onRetry={audit.reload} />
      {!audit.data && !audit.error && <Loading />}
      {audit.data && rows.length === 0 && <Empty>No entries{filter ? ' match the filter' : ''}.</Empty>}
      {rows.length > 0 && (
        <>
          <p className="muted small">
            Showing {rows.length} of the latest {audit.data?.length ?? 0} entries.
          </p>
          <DataTable
            head={['Time', 'Actor', 'Action', 'Target', 'Detail']}
            rows={rows.map((e) => [
              <span title={e.ts}>{fmtTime(e.ts)}</span>,
              e.actor,
              <code className="small">{e.action}</code>,
              e.target ? <code className="small">{e.target}</code> : '',
              e.detail,
            ])}
          />
        </>
      )}
    </Card>
  );
}
