import { useEffect, useMemo, useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { estimateSeconds } from '../lib/domain';
import { fmtDuration } from '../lib/format';
import { useApi, useDocumentTitle } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { Link, navigate, useQuery } from '../lib/router';
import type { Agent } from '../lib/types';
import { Button, Card, Empty, ErrorBox, Field, Loading, Notice, PageHeader, Segmented, StatusDot } from '../components/ui';
import { useProfiles } from '../components/shared';
import { agentState } from '../lib/domain';

const PROFILE_TEXT: Record<string, string> = {
  quick: 'Idle latency, TCP throughput both ways and bufferbloat. A fast health check.',
  standard: 'Adds the path in both directions, single-stream TCP, UDP loss/jitter, path MTU, middlebox checks and a voice-call simulation.',
  full: 'Everything: stream scaling, rate and packet-size sweeps, QoS priority, congestion-control comparison, short flows, throttling checks and MTR.',
};

// Group tests for the custom picker.
const GROUPS: { label: string; tests: string[] }[] = [
  { label: 'Basics', tests: ['info', 'latency'] },
  { label: 'Throughput', tests: ['tcp-down', 'tcp-up', 'tcp-down-1', 'tcp-up-1', 'streams-down', 'streams-up', 'cc'] },
  { label: 'Loss, jitter and queueing', tests: ['udp-down', 'udp-up', 'bufferbloat', 'sweep-down', 'sweep-up', 'sizesweep-up', 'sizesweep-down'] },
  { label: 'Path', tests: ['trace', 'mtr', 'pmtu', 'integrity'] },
  { label: 'Applications', tests: ['voip', 'voip-loaded', 'qos', 'shortflow', 'discrimination'] },
  { label: 'Shaping', tests: ['sqm'] },
];

export function NewRun() {
  useDocumentTitle('New run');
  const canRun = useCan('operator');
  const [q] = useQuery();
  const agents = useApi<Agent[]>('/api/agents');
  const profiles = useProfiles();
  useLiveRefresh((m) => m.type === 'agent_status' || m.type === 'run_status', agents.reload);

  const [agentId, setAgentId] = useState(q.get('agent') ?? '');
  const [mode, setMode] = useState<'profile' | 'custom'>(q.get('tests') ? 'custom' : 'profile');
  const [profile, setProfile] = useState(q.get('profile') ?? 'quick');
  const [tests, setTests] = useState<Set<string>>(() => new Set((q.get('tests') ?? 'info,latency,tcp-down,tcp-up').split(',').filter(Boolean)));
  const [scale, setScale] = useState('1');
  const [maxRate, setMaxRate] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const usable = (agents.data ?? []).filter((a) => !a.revoked);
  const selected = usable.find((a) => a.id === agentId);

  // Default to the first online agent.
  useEffect(() => {
    if (!agentId && agents.data) {
      const first = agents.data.find((a) => a.online && !a.busy && !a.revoked);
      if (first) setAgentId(first.id);
    }
  }, [agents.data, agentId]);

  const testList = useMemo(() => {
    if (mode === 'profile') return profiles?.profiles[profile] ?? [];
    // Keep the engine's canonical order.
    return (profiles?.tests ?? []).filter((t) => tests.has(t));
  }, [mode, profile, tests, profiles]);

  const scaleN = Number(scale);
  const est = estimateSeconds(testList, Number.isFinite(scaleN) && scaleN > 0 ? scaleN : 1);

  const toggle = (t: string) => {
    const n = new Set(tests);
    if (n.has(t)) n.delete(t);
    else n.add(t);
    setTests(n);
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!selected) return setError('Choose an agent.');
    if (!selected.online) return setError(`${selected.name} is offline.`);
    if (selected.busy) return setError(`${selected.name} is already running a test.`);
    if (!testList.length) return setError('Choose at least one test.');
    if (!Number.isFinite(scaleN) || scaleN < 0.1 || scaleN > 10) return setError('Scale must be between 0.1 and 10.');
    const mr = maxRate.trim() ? Number(maxRate) : 0;
    if (!Number.isFinite(mr) || mr < 0) return setError('Max rate must be a positive number.');
    setBusy(true);
    try {
      const body: Record<string, unknown> = { agent_id: selected.id, profile: mode === 'profile' ? profile : 'custom', scale: scaleN };
      if (mode === 'custom') body.tests = testList;
      if (mr > 0) body.max_rate_mbps = mr;
      const r = await api.post<{ run_id: string }>('/api/runs', body);
      navigate(`/runs/${r.run_id}`);
    } catch (err) {
      setError(errorMessage(err));
      agents.reload();
    } finally {
      setBusy(false);
    }
  };

  if (!canRun)
    return (
      <div className="page">
        <PageHeader title="New run" />
        <Notice kind="info">Starting runs needs the operator role.</Notice>
      </div>
    );

  return (
    <div className="page">
      <PageHeader title="New run" sub="Run a test profile between an agent and this server." />
      <form onSubmit={submit} className="newrun">
        <Card title="1. Agent">
          <ErrorBox error={agents.error} onRetry={agents.reload} />
          {!agents.data && !agents.error && <Loading />}
          {agents.data && usable.length === 0 && (
            <Empty>
              No agents are enrolled. <Link to="/agents">Add an agent</Link> first.
            </Empty>
          )}
          <div className="choice-grid" role="radiogroup" aria-label="Agent">
            {usable.map((a) => {
              const st = agentState(a);
              const ok = a.online && !a.busy;
              return (
                <label key={a.id} className={`choice${agentId === a.id ? ' on' : ''}${ok ? '' : ' off'}`}>
                  <input type="radio" name="agent" value={a.id} checked={agentId === a.id} disabled={!ok} onChange={() => setAgentId(a.id)} />
                  <span className="choice-title">{a.name}</span>
                  <span className="choice-sub">{a.site || a.observed_addr || a.id}</span>
                  <StatusDot kind={st.kind} label={a.busy ? 'Busy (running a test)' : st.label} icon={st.icon} />
                </label>
              );
            })}
          </div>
        </Card>

        <Card title="2. Tests" actions={<Segmented label="Test selection" options={[{ id: 'profile', label: 'Profile' }, { id: 'custom', label: 'Custom' }]} value={mode} onChange={setMode} />}>
          {!profiles && <Loading />}
          {profiles && mode === 'profile' && (
            <div className="choice-grid" role="radiogroup" aria-label="Profile">
              {['quick', 'standard', 'full'].filter((p) => profiles.profiles[p]).map((p) => (
                <label key={p} className={`choice${profile === p ? ' on' : ''}`}>
                  <input type="radio" name="profile" value={p} checked={profile === p} onChange={() => setProfile(p)} />
                  <span className="choice-title">
                    {p[0].toUpperCase() + p.slice(1)} <span className="muted">· {profiles.profiles[p].length} tests</span>
                  </span>
                  <span className="choice-sub">{PROFILE_TEXT[p]}</span>
                  <span className="choice-sub">about {fmtDuration(estimateSeconds(profiles.profiles[p], Number.isFinite(scaleN) && scaleN > 0 ? scaleN : 1))}</span>
                </label>
              ))}
            </div>
          )}
          {profiles && mode === 'profile' && (
            <details className="table-toggle">
              <summary>Tests in the {profile} profile</summary>
              <ol className="test-list">
                {(profiles.profiles[profile] ?? []).map((t) => (
                  <li key={t}>
                    {profiles.titles[t] ?? t} <code className="small muted">{t}</code>
                  </li>
                ))}
              </ol>
            </details>
          )}
          {profiles && mode === 'custom' && (
            <div className="test-groups">
              {GROUPS.map((g) => {
                const list = g.tests.filter((t) => profiles.tests.includes(t));
                if (!list.length) return null;
                return (
                  <fieldset key={g.label} className="test-group">
                    <legend>{g.label}</legend>
                    {list.map((t) => (
                      <label key={t} className="check">
                        <input type="checkbox" checked={tests.has(t)} onChange={() => toggle(t)} />
                        <span>
                          {profiles.titles[t] ?? t} <code className="small muted">{t}</code>
                        </span>
                      </label>
                    ))}
                  </fieldset>
                );
              })}
              {/* Any test the server knows that is not in a group above. */}
              {profiles.tests.some((t) => !GROUPS.some((g) => g.tests.includes(t))) && (
                <fieldset className="test-group">
                  <legend>Other</legend>
                  {profiles.tests
                    .filter((t) => !GROUPS.some((g) => g.tests.includes(t)))
                    .map((t) => (
                      <label key={t} className="check">
                        <input type="checkbox" checked={tests.has(t)} onChange={() => toggle(t)} />
                        <span>{profiles.titles[t] ?? t}</span>
                      </label>
                    ))}
                </fieldset>
              )}
              <div className="row-actions">
                <Button small variant="ghost" onClick={() => setTests(new Set())}>
                  Clear
                </Button>
                <Button small variant="ghost" onClick={() => setTests(new Set(profiles.profiles.quick ?? []))}>
                  Select the quick profile
                </Button>
              </div>
            </div>
          )}
        </Card>

        <Card title="3. Options">
          <div className="form-row">
            <Field label="Scale (duration multiplier)" hint="1 = normal. 0.3 runs each timed test for 30% of its normal duration.">
              <input type="number" min={0.1} max={10} step={0.1} value={scale} onChange={(e) => setScale(e.target.value)} />
            </Field>
            <Field label="Max rate (Mbit/s)" hint="Caps UDP tests and sweeps. Empty = automatic (from TCP results, within the agent's policy).">
              <input type="number" min={0} step="any" value={maxRate} onChange={(e) => setMaxRate(e.target.value)} placeholder="automatic" />
            </Field>
          </div>
        </Card>

        <div className="submit-bar">
          <div>
            <strong>{testList.length} tests</strong>
            <span className="muted"> · estimated duration about {fmtDuration(est)}</span>
            {selected && <span className="muted"> · on {selected.name}</span>}
          </div>
          <ErrorBox error={error} />
          <Button type="submit" variant="primary" busy={busy} disabled={!selected || !selected.online || selected.busy || !testList.length}>
            Start run
          </Button>
        </div>
      </form>
    </div>
  );
}
