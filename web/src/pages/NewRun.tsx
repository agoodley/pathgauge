import { useEffect, useMemo, useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { estimateSeconds } from '../lib/domain';
import { f1, f2, fmtDuration, plural } from '../lib/format';
import {
  DEFAULT_SCENARIO,
  MIX_KINDS,
  MIX_SCENARIOS,
  describeCounts,
  formatSpec,
  isRealtime,
  parseSpec,
  realtimeMbps,
  scenarioCounts,
  validateCounts,
  type MixCounts,
} from '../lib/mix';
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
  full: 'Everything: stream scaling, rate and packet-size sweeps, QoS priority, a traffic mix of calls, browsing and transfers, congestion-control comparison, short flows, throttling checks and MTR.',
};

// Group tests for the custom picker.
const GROUPS: { label: string; tests: string[] }[] = [
  { label: 'Basics', tests: ['info', 'latency'] },
  { label: 'Throughput', tests: ['tcp-down', 'tcp-up', 'tcp-down-1', 'tcp-up-1', 'streams-down', 'streams-up', 'cc'] },
  { label: 'Loss, jitter and queueing', tests: ['udp-down', 'udp-up', 'bufferbloat', 'sweep-down', 'sweep-up', 'sizesweep-up', 'sizesweep-down'] },
  { label: 'Path', tests: ['trace', 'mtr', 'pmtu', 'integrity'] },
  { label: 'Applications', tests: ['voip', 'voip-loaded', 'qos', 'shortflow', 'discrimination'] },
  { label: 'Traffic mix and call capacity', tests: ['mix', 'capacity-voice', 'capacity-video'] },
  { label: 'Shaping', tests: ['sqm'] },
];

const CAPACITY_TESTS = ['capacity-voice', 'capacity-video'];

function countsToRaw(c: MixCounts): Record<string, string> {
  return Object.fromEntries(MIX_KINDS.map((k) => [k.name, String(c[k.name] ?? 0)]));
}

/** Initial mix choice from ?mix= (a scenario name or a kind:count list). */
function initialMix(q: string | null): { scenario: string; raw: Record<string, string> } {
  const v = (q ?? '').trim().toLowerCase();
  if (MIX_SCENARIOS.some((s) => s.id === v)) return { scenario: v, raw: countsToRaw(scenarioCounts(v)) };
  const parsed = v ? parseSpec(v) : null;
  if (parsed) return { scenario: 'custom', raw: countsToRaw(parsed) };
  return { scenario: DEFAULT_SCENARIO, raw: countsToRaw(scenarioCounts(DEFAULT_SCENARIO)) };
}

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
  const [mixInit] = useState(() => initialMix(q.get('mix')));
  const [scenario, setScenario] = useState(mixInit.scenario);
  const [mixRaw, setMixRaw] = useState<Record<string, string>>(mixInit.raw);
  const [dscp, setDscp] = useState(q.get('dscp') !== '0');
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

  const hasMix = testList.includes('mix');
  const hasCalls = hasMix || testList.some((t) => CAPACITY_TESTS.includes(t));
  const custom = validateCounts(mixRaw);
  const mixCounts = scenario === 'custom' ? custom.counts : scenarioCounts(scenario);
  const mixError = hasMix && scenario === 'custom' ? custom.error : null;

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
    if (mixError) return setError(`Traffic mix: ${mixError}`);
    if (hasMix && mr > 0 && realtimeMbps(mixCounts) > mr)
      return setError(
        `The calls and games in this mix need about ${f2(realtimeMbps(mixCounts))} Mbit/s each way, more than the ${mr} Mbit/s max rate. Raise the max rate or choose fewer calls.`,
      );
    setBusy(true);
    try {
      const body: Record<string, unknown> = { agent_id: selected.id, profile: mode === 'profile' ? profile : 'custom', scale: scaleN };
      if (mode === 'custom') body.tests = testList;
      if (mr > 0) body.max_rate_mbps = mr;
      if (hasMix) body.mix = scenario === 'custom' ? formatSpec(mixCounts) : scenario;
      if (hasCalls && !dscp) body.no_dscp = true;
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

        {hasCalls && (
          <Card title="3. Traffic mix and calls">
            {hasMix && (
              <MixChooser
                scenario={scenario}
                setScenario={(id) => {
                  // Custom starts from the scenario that was selected.
                  if (id === 'custom' && scenario !== 'custom') setMixRaw(countsToRaw(scenarioCounts(scenario)));
                  setScenario(id);
                }}
                raw={mixRaw}
                setRaw={setMixRaw}
                counts={mixCounts}
                error={mixError}
              />
            )}
            <div className="mix-dscp">
              <label className="check">
                <input type="checkbox" checked={dscp} onChange={(e) => setDscp(e.target.checked)} aria-describedby="mix-dscp-hint" />
                <span>Mark calls with DSCP (EF voice, AF41 video)</span>
              </label>
              <p className="field-hint" id="mix-dscp-hint">
                Simulated calls are marked as calling apps mark them. Untick to see how calls fare with no priority marking
                {hasMix && testList.some((t) => CAPACITY_TESTS.includes(t)) ? ' (applies to the traffic mix and the call capacity tests)' : ''}.
              </p>
            </div>
          </Card>
        )}

        <Card title={hasCalls ? '4. Options' : '3. Options'}>
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
          <Button type="submit" variant="primary" busy={busy} disabled={!selected || !selected.online || selected.busy || !testList.length || !!mixError}>
            Start run
          </Button>
        </div>
      </form>
    </div>
  );
}

/** Scenario picker and custom builder for the traffic-mix test. */
function MixChooser({
  scenario,
  setScenario,
  raw,
  setRaw,
  counts,
  error,
}: {
  scenario: string;
  setScenario: (id: string) => void;
  raw: Record<string, string>;
  setRaw: (r: Record<string, string>) => void;
  counts: MixCounts;
  error: string | null;
}) {
  const total = Object.values(counts).reduce((a, n) => a + n, 0);
  const rt = MIX_KINDS.filter(isRealtime).reduce((a, k) => a + (counts[k.name] ?? 0), 0);
  const transfers = (counts.download ?? 0) + (counts.upload ?? 0);
  const groups: { label: string; kinds: string[] }[] = [
    { label: 'Calls and games', kinds: ['voice', 'voice-opus', 'video', 'video-hd', 'gaming'] },
    { label: 'Browsing and streaming', kinds: ['web', 'stream', 'stream-4k'] },
    { label: 'File transfers', kinds: ['download', 'upload'] },
  ];
  return (
    <div className="mix-chooser">
      <div className="choice-grid" role="radiogroup" aria-label="Traffic mix">
        {MIX_SCENARIOS.map((s) => {
          const c = scenarioCounts(s.id);
          const n = Object.values(c).reduce((a, x) => a + x, 0);
          return (
            <label key={s.id} className={`choice${scenario === s.id ? ' on' : ''}`}>
              <input type="radio" name="mix" value={s.id} checked={scenario === s.id} onChange={() => setScenario(s.id)} />
              <span className="choice-title">
                {s.label} <span className="muted">· {plural(n, 'session')}</span>
              </span>
              <span className="choice-sub">{s.text}</span>
              <span className="choice-sub muted">{describeCounts(c)}</span>
            </label>
          );
        })}
        <label className={`choice${scenario === 'custom' ? ' on' : ''}`}>
          <input type="radio" name="mix" value="custom" checked={scenario === 'custom'} onChange={() => setScenario('custom')} />
          <span className="choice-title">Custom</span>
          <span className="choice-sub">Choose how many sessions of each kind to run at once.</span>
        </label>
      </div>

      {scenario === 'custom' && (
        <div className="mix-builder">
          {groups.map((g) => (
            <fieldset key={g.label} className="test-group">
              <legend>{g.label}</legend>
              {g.kinds.map((name) => {
                const k = MIX_KINDS.find((x) => x.name === name)!;
                const v = raw[name] ?? '';
                const n = Number(v);
                const bad = v.trim() !== '' && (!Number.isInteger(n) || n < 0 || n > k.max);
                return (
                  <div key={name} className="mix-count">
                    <label className="mix-count-label" htmlFor={`mix-count-${name}`}>
                      {k.label}
                    </label>
                    <input
                      id={`mix-count-${name}`}
                      type="number"
                      inputMode="numeric"
                      min={0}
                      max={k.max}
                      step={1}
                      value={v}
                      aria-invalid={bad || undefined}
                      aria-describedby={`mix-max-${name}`}
                      onChange={(e) => setRaw({ ...raw, [name]: e.target.value })}
                    />
                    <span className="field-hint" id={`mix-max-${name}`}>
                      max {k.max}
                    </span>
                  </div>
                );
              })}
            </fieldset>
          ))}
          <div className="row-actions">
            {MIX_SCENARIOS.map((s) => (
              <Button key={s.id} small variant="ghost" onClick={() => setRaw(countsToRaw(scenarioCounts(s.id)))}>
                Start from {s.label.toLowerCase()}
              </Button>
            ))}
            <Button small variant="ghost" onClick={() => setRaw(countsToRaw({}))}>
              Clear
            </Button>
          </div>
        </div>
      )}

      <div aria-live="polite">
        {error ? (
          <Notice kind="critical">{error}</Notice>
        ) : (
          total > 0 && (
            <p className="mix-summary">
              <strong>This mix:</strong> {describeCounts(counts)}.{' '}
              <span className="muted">
                {rt > 0 && <>The calls and games need about {f1(realtimeMbps(counts))} Mbit/s each way. </>}
                {transfers > 0 && total > transfers
                  ? 'With file transfers the mix runs twice: first without them, then with them.'
                  : ''}
              </span>
            </p>
          )
        )}
      </div>
    </div>
  );
}
