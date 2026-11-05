import { useEffect, useMemo, useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import { useCan } from '../lib/auth';
import { estimateSeconds, groupedTestSeconds, groupSeconds } from '../lib/domain';
import { f1, f2, fmtDuration, fmtMbps, plural } from '../lib/format';
import {
  CDP_DEFAULTS,
  CDP_FALLBACK_RATE,
  CDP_FIELDS,
  CDP_TESTS,
  cdpDefaultDuration,
  cdpRates,
  cdpShape,
  parseCDP,
  validateCDP,
  type CDPKey,
  type CDPRaw,
} from '../lib/cdp';
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
import { NOT_IN_GROUPS, groupDefaultDuration, groupError, groupMembers, isGroup } from '../lib/groups';
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
  { label: 'Replication', tests: ['cdp-up', 'cdp-down'] },
  { label: 'Shaping', tests: ['sqm'] },
];

// The engine names the CDP tests by "this side" and "the other side"; on
// this page the two sides are the agent and the server.
const PICKER_TITLES: Record<string, string> = {
  'cdp-up': 'CDP replication, agent to server (the agent is the source)',
  'cdp-down': 'CDP replication, server to agent (the server is the source)',
};

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
  const [tests, setTests] = useState<Set<string>>(
    () => new Set((q.get('tests') ?? 'info,latency,tcp-down,tcp-up').split(',').filter((t) => t && !isGroup(t))),
  );
  // Groups ("a+b" entries in ?tests=): tests run at the same time.
  const [groups, setGroups] = useState<string[][]>(() => (q.get('tests') ?? '').split(',').filter(isGroup).map(groupMembers));
  const [groupDur, setGroupDur] = useState(q.get('gdur') ?? '');
  const [baseline, setBaseline] = useState(q.get('baseline') !== '0');
  const [scale, setScale] = useState('1');
  const [maxRate, setMaxRate] = useState('');
  const [mixInit] = useState(() => initialMix(q.get('mix')));
  const [scenario, setScenario] = useState(mixInit.scenario);
  const [mixRaw, setMixRaw] = useState<Record<string, string>>(mixInit.raw);
  const [dscp, setDscp] = useState(q.get('dscp') !== '0');
  const [cdpRaw, setCdpRaw] = useState<CDPRaw>(() => parseCDP(q.get('cdp')));
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

  // Tests run one at a time, in the engine's canonical order.
  const singles = useMemo(() => {
    if (mode === 'profile') return profiles?.profiles[profile] ?? [];
    return (profiles?.tests ?? []).filter((t) => tests.has(t));
  }, [mode, profile, tests, profiles]);
  // Complete groups go after them as "a+b+c" entries; empty ones are ignored.
  const groupEntries = groups.filter((g) => g.length && !groupError(g)).map((g) => g.join('+'));
  const groupProblem = groups.map((g, i) => (g.length && groupError(g) ? `Group ${i + 1}: ${groupError(g)}.` : '')).find(Boolean) ?? null;
  const testList = [...singles, ...groupEntries];
  // Every test that runs, grouped or not (for the option cards).
  const allTests = useMemo(() => [...new Set([...singles, ...groups.flat()])], [singles, groups]);

  const scaleN = Number(scale);
  const scaleOK = Number.isFinite(scaleN) && scaleN > 0 ? scaleN : 1;
  const gdN = groupDur.trim() ? Number(groupDur) : 0;
  const gdBad = groupEntries.length > 0 && (!Number.isFinite(gdN) || gdN < 0);
  const hasCDP = allTests.some((t) => CDP_TESTS.includes(t));
  const cdp = validateCDP(cdpRaw);
  const cdpError = hasCDP ? cdp.error : null;
  const est = estimateSeconds(testList, scaleOK, {
    cdpDurationS: hasCDP ? cdp.settings.duration_s : undefined,
    groupDurationS: gdBad ? undefined : gdN,
    baseline,
  });
  const mrN = maxRate.trim() ? Number(maxRate) : 0;
  const cdpCheck = cdpPlan(allTests, cdp.settings, Number.isFinite(mrN) && mrN > 0 ? mrN : 0);

  const hasMix = allTests.includes('mix');
  const hasCalls = hasMix || allTests.some((t) => CAPACITY_TESTS.includes(t));
  const custom = validateCounts(mixRaw);
  const mixCounts = scenario === 'custom' ? custom.counts : scenarioCounts(scenario);
  const mixError = hasMix && scenario === 'custom' ? custom.error : null;

  // Optional cards between the tests and the options are numbered in order.
  const cardNo = { calls: 3, cdp: hasCalls ? 4 : 3, options: 3 + (hasCalls ? 1 : 0) + (hasCDP ? 1 : 0) };

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
    if (groupProblem) return setError(groupProblem);
    if (gdBad) return setError('Group duration must be a positive number of seconds.');
    if (!Number.isFinite(scaleN) || scaleN < 0.1 || scaleN > 10) return setError('Scale must be between 0.1 and 10.');
    const mr = maxRate.trim() ? Number(maxRate) : 0;
    if (!Number.isFinite(mr) || mr < 0) return setError('Max rate must be a positive number.');
    if (mixError) return setError(`Traffic mix: ${mixError}`);
    if (hasMix && mr > 0 && realtimeMbps(mixCounts) > mr)
      return setError(
        `The calls and games in this mix need about ${f2(realtimeMbps(mixCounts))} Mbit/s each way, more than the ${mr} Mbit/s max rate. Raise the max rate or choose fewer calls.`,
      );
    if (cdpError) return setError(`CDP replication: ${cdpError}`);
    if (hasCDP && cdpCheck.overCap) return setError(`CDP replication: ${cdpCheck.overCap}`);
    setBusy(true);
    try {
      const body: Record<string, unknown> = { agent_id: selected.id, profile: mode === 'profile' ? profile : 'custom', scale: scaleN };
      // With groups the list is sent even for a profile (the run becomes custom).
      if (mode === 'custom' || groupEntries.length) body.tests = testList;
      if (groupEntries.length && gdN > 0) body.group_duration_s = gdN;
      if (groupEntries.length && !baseline) body.no_baseline = true;
      if (mr > 0) body.max_rate_mbps = mr;
      if (hasMix) body.mix = scenario === 'custom' ? formatSpec(mixCounts) : scenario;
      if (hasCalls && !dscp) body.no_dscp = true;
      // Only the settings filled in; the engine fills the rest.
      if (hasCDP && Object.keys(cdp.settings).length) body.cdp = cdp.settings;
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
                  <span className="choice-sub">about {fmtDuration(estimateSeconds(profiles.profiles[p], scaleOK))}</span>
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
                    {PICKER_TITLES[t] ?? profiles.titles[t] ?? t} <code className="small muted">{t}</code>
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
                          {PICKER_TITLES[t] ?? profiles.titles[t] ?? t} <code className="small muted">{t}</code>
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
          {profiles && (
            <GroupBuilder
              groups={groups}
              setGroups={setGroups}
              order={profiles.tests}
              title={(t) => PICKER_TITLES[t] ?? profiles.titles[t] ?? t}
              scale={scaleOK}
              dur={groupDur}
              setDur={setGroupDur}
              durBad={gdBad}
              baseline={baseline}
              setBaseline={setBaseline}
            />
          )}
        </Card>

        {hasCalls && (
          <Card title={`${cardNo.calls}. Traffic mix and calls`}>
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
                {hasMix && allTests.some((t) => CAPACITY_TESTS.includes(t)) ? ' (applies to the traffic mix and the call capacity tests)' : ''}.
              </p>
            </div>
          </Card>
        )}

        {hasCDP && (
          <Card title={`${cardNo.cdp}. CDP replication`}>
            <CDPOptions
              raw={cdpRaw}
              setRaw={setCdpRaw}
              bad={cdp.bad}
              error={cdpError}
              plan={cdpCheck}
              tests={allTests.filter((t) => CDP_TESTS.includes(t))}
              grouped={CDP_TESTS.filter((t) => groups.some((g) => g.includes(t)))}
              defaultDuration={cdpDefaultDuration(scaleOK)}
            />
          </Card>
        )}

        <Card title={`${cardNo.options}. Options`}>
          <div className="form-row">
            <Field label="Scale (duration multiplier)" hint="1 = normal. 0.3 runs each timed test for 30% of its normal duration.">
              <input type="number" min={0.1} max={10} step={0.1} value={scale} onChange={(e) => setScale(e.target.value)} />
            </Field>
            <Field
              label="Max rate (Mbit/s)"
              hint={`Caps UDP tests and sweeps${hasCDP ? '; CDP bursts must fit under it' : ''}. Empty = automatic (from TCP results, within the agent's policy).`}
            >
              <input type="number" min={0} step="any" value={maxRate} onChange={(e) => setMaxRate(e.target.value)} placeholder="automatic" />
            </Field>
          </div>
        </Card>

        <div className="submit-bar">
          <div>
            <strong>
              {plural(singles.length, 'test')}
              {groupEntries.length > 0 && ` + ${plural(groupEntries.length, 'group')} run together`}
            </strong>
            <span className="muted"> · estimated duration about {fmtDuration(est)}</span>
            {selected && <span className="muted"> · on {selected.name}</span>}
          </div>
          <ErrorBox error={error} />
          <Button type="submit" variant="primary" busy={busy} disabled={!selected || !selected.online || selected.busy || !testList.length || !!mixError || !!cdpError || !!groupProblem || gdBad}>
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

/** A number as "3", "1.5" or "0.67". */
const n2 = (x: number) => String(+x.toFixed(2));

interface CDPPlan {
  shape: ReturnType<typeof cdpShape>;
  /** Average rate when it is known before the run (set, or the fallback). */
  avg: number | null;
  /** Rates between and during bursts (Mbit/s, or × the average when avg is null). */
  base: number;
  peak: number;
  limit: number;
  /** Why the bursts can't run under the max rate (the engine refuses). */
  overCap: string | null;
}

/**
 * What the CDP settings produce. Without a rate the engine takes half of
 * what an earlier TCP test in the same direction measured, else 20 Mbit/s.
 */
function cdpPlan(tests: string[], s: ReturnType<typeof validateCDP>['settings'], maxRate: number): CDPPlan {
  const shape = cdpShape(s);
  const tcpFirst = (t: string) => (t === 'cdp-up' ? ['tcp-up', 'tcp-up-1'] : ['tcp-down', 'tcp-down-1']).some((x) => tests.includes(x));
  const fromTcp = !s.rate_mbps && tests.some((t) => CDP_TESTS.includes(t) && tcpFirst(t));
  const avg = s.rate_mbps ?? (fromTcp ? null : CDP_FALLBACK_RATE);
  const { base, peak } = cdpRates(avg ?? 1, shape);
  const limit = s.limit_mbps ?? 0;
  let overCap: string | null = null;
  if (avg !== null && maxRate > 0) {
    const offered = limit > 0 ? Math.min(peak, limit) : peak;
    if (offered > maxRate + 1e-9)
      overCap = `Bursts reach ${fmtMbps(offered)}, above the ${maxRate} Mbit/s max rate, so the test would stop with an error. Lower the change rate or burst factor, set a throttle of at most ${maxRate} Mbit/s, or raise the max rate.`;
  }
  return { shape, avg, base, peak, limit, overCap };
}

const CDP_SOURCE: Record<string, string> = {
  'cdp-up': 'the agent is the source and replicates to the server',
  'cdp-down': 'the server is the source and replicates to the agent',
};

/** Settings of the CDP replication tests, with what they produce. */
function CDPOptions({
  raw,
  setRaw,
  bad,
  error,
  plan,
  tests,
  grouped,
  defaultDuration,
}: {
  raw: CDPRaw;
  setRaw: (r: CDPRaw) => void;
  bad: Set<CDPKey>;
  error: string | null;
  plan: CDPPlan;
  tests: string[];
  /** CDP tests that are in a group (they run for the group duration). */
  grouped: string[];
  defaultDuration: number;
}) {
  const { shape, avg } = plan;
  const placeholder = (k: CDPKey): string => {
    switch (k) {
      case 'rate_mbps':
        return avg === null ? 'half the TCP rate' : String(CDP_FALLBACK_RATE);
      case 'limit_mbps':
        return 'none';
      case 'duration_s':
        return n2(defaultDuration);
    }
    return String(CDP_DEFAULTS[k as keyof typeof CDP_DEFAULTS]);
  };
  const filled = Object.values(raw).some((v) => (v ?? '').trim() !== '');
  return (
    <div className="cdp-options">
      <p className="cdp-intro">
        Simulates continuous data protection replication (Veeam CDP and similar): changed data is produced at an average rate with periodic write bursts and sent
        to the other site as fast as the path allows. The test measures the <strong>lag</strong>, the age of the oldest change not yet sent: the RPO the replica
        would have had.
      </p>
      <p className="field-hint cdp-dirs">
        {tests.map((t, i) => (
          <span key={t}>
            {i > 0 && ' '}
            <code className="small">{t}</code>: {CDP_SOURCE[t]}.
          </span>
        ))}
      </p>
      <div className="cdp-fields">
        {CDP_FIELDS.map((f) => (
          <Field key={f.key} label={f.label} hint={f.hint}>
            <input
              type="number"
              inputMode={f.integer ? 'numeric' : 'decimal'}
              min={0}
              max={f.max}
              step={f.integer ? 1 : 'any'}
              value={raw[f.key] ?? ''}
              placeholder={placeholder(f.key)}
              aria-invalid={bad.has(f.key) || undefined}
              onChange={(e) => setRaw({ ...raw, [f.key]: e.target.value })}
            />
          </Field>
        ))}
      </div>
      <div className="cdp-readout" aria-live="polite">
        {error ? (
          <Notice kind="critical">{error}</Notice>
        ) : (
          <>
            <p className="cdp-rates">
              {avg !== null ? (
                <>
                  <strong>Changed data:</strong> {plan.base > 0 ? `${fmtMbps(plan.base)} between bursts` : 'no changes between bursts'} and {fmtMbps(plan.peak)} during{' '}
                  {n2(shape.burstS)} s bursts every {n2(shape.periodS)} s (average {fmtMbps(avg)}).
                </>
              ) : (
                <>
                  <strong>Changed data:</strong> {n2(plan.base)} × the average between bursts and {n2(plan.peak)} × during {n2(shape.burstS)} s bursts every{' '}
                  {n2(shape.periodS)} s. The average is half what TCP measured, so bursts run at about {n2(plan.peak / 2)} × the measured TCP rate.
                </>
              )}
            </p>
            {shape.burstClamped && <p className="field-hint">A burst can’t be longer than the period, so changes are produced at a steady rate.</p>}
            {shape.factorClamped && !shape.burstClamped && (
              <p className="field-hint">The burst factor is capped at {n2(shape.factor)} (period ÷ burst length) so the rate between bursts doesn’t go below zero.</p>
            )}
            {avg !== null && plan.limit > 0 && plan.limit < avg && (
              <p className="field-hint">The throttle is below the average change rate, so replication falls further behind for the whole run.</p>
            )}
            {avg !== null && plan.limit >= avg && plan.limit < plan.peak && (
              <p className="field-hint">The throttle holds bursts to {fmtMbps(plan.limit)}: changes queue during each burst and catch up between them.</p>
            )}
            {grouped.length > 0 && (
              <p className="field-hint">In a group, {grouped.join(' and ')} runs for the group duration rather than the duration set here.</p>
            )}
            {plan.overCap && <Notice kind="warning">{plan.overCap}</Notice>}
          </>
        )}
      </div>
      {filled && (
        <div className="row-actions">
          <Button small variant="ghost" onClick={() => setRaw({})}>
            Use the defaults
          </Button>
        </div>
      )}
    </div>
  );
}

/** Groups of tests to run at the same time, with their duration and baseline. */
function GroupBuilder({
  groups,
  setGroups,
  order,
  title,
  scale,
  dur,
  setDur,
  durBad,
  baseline,
  setBaseline,
}: {
  groups: string[][];
  setGroups: (g: string[][]) => void;
  /** Every test the server knows, in the engine's order. */
  order: string[];
  title: (t: string) => string;
  scale: number;
  dur: string;
  setDur: (v: string) => void;
  durBad: boolean;
  baseline: boolean;
  setBaseline: (v: boolean) => void;
}) {
  const d = Number(dur) > 0 ? Number(dur) : groupDefaultDuration(scale);
  const set = (gi: number, g: string[]) => setGroups(groups.map((x, i) => (i === gi ? g : x)));
  // Members stay in the engine's order, as in the picker.
  const add = (gi: number, t: string) => set(gi, order.filter((x) => x === t || groups[gi].includes(x)));
  const others = order.filter((t) => !GROUPS.some((c) => c.tests.includes(t)));
  const cats = [...GROUPS, ...(others.length ? [{ label: 'Other', tests: others }] : [])];
  return (
    <section className="together" aria-labelledby="together-h">
      <h3 id="together-h">Run together</h3>
      <p className="together-intro">
        Tests in a group start at the same time and each runs for the group duration, so you can see what they do to each other, for example a voice call while
        replication runs.
      </p>
      {groups.map((g, gi) => {
        const n = gi + 1;
        const long = g.filter((t) => groupedTestSeconds(t, d) > 2 * d);
        const err = g.length ? groupError(g) : null;
        return (
          <fieldset key={gi} className="group-box">
            <legend>Group {n}</legend>
            <div className="group-row">
              {g.length > 0 ? (
                <ul className="group-members" aria-label={`Tests in group ${n}`}>
                  {g.map((t) => (
                    <li key={t} className="member">
                      <span>{title(t)}</span> <code className="small muted">{t}</code>
                      <button type="button" className="member-x" aria-label={`Remove ${title(t)} from group ${n}`} onClick={() => set(gi, g.filter((x) => x !== t))}>
                        ×
                      </button>
                    </li>
                  ))}
                </ul>
              ) : (
                <span className="muted small">No tests yet.</span>
              )}
              <select aria-label={`Add a test to group ${n}`} value="" onChange={(e) => e.target.value && add(gi, e.target.value)}>
                <option value="">Add a test…</option>
                {cats.map((c) => {
                  const list = c.tests.filter((t) => order.includes(t));
                  if (!list.length) return null;
                  return (
                    <optgroup key={c.label} label={c.label}>
                      {list.map((t) => (
                        <option key={t} value={t} disabled={!!NOT_IN_GROUPS[t] || g.includes(t)}>
                          {title(t)}
                          {NOT_IN_GROUPS[t] ? ` (can’t run in a group: ${NOT_IN_GROUPS[t]})` : g.includes(t) ? ' (in this group)' : ''}
                        </option>
                      ))}
                    </optgroup>
                  );
                })}
              </select>
              <Button small variant="ghost" onClick={() => setGroups(groups.filter((_, i) => i !== gi))}>
                Remove group {n}
              </Button>
            </div>
            {err ? (
              <p className="group-error">
                <span aria-hidden="true">✖</span> {err === 'add at least two tests' ? 'Add at least one more test: a group runs two or more tests together.' : `${err[0].toUpperCase()}${err.slice(1)}.`}
              </p>
            ) : (
              g.length > 1 && (
                <p className="field-hint">
                  About {fmtDuration(groupSeconds(g, d, baseline))}: {baseline ? `each test on its own for ${n2(d)} s, then ` : ''}all {g.length} together for {n2(d)}{' '}
                  s.
                  {long.length > 0 && ` ${long.map(title).join(' and ')} ${long.length > 1 ? 'run' : 'runs'} in phases of that length, so ${long.length > 1 ? 'they take' : 'it takes'} longer.`}
                </p>
              )
            )}
          </fieldset>
        );
      })}
      <div className="row-actions">
        <Button small onClick={() => setGroups([...groups, []])}>
          {groups.length ? 'Add another group' : 'Add a group'}
        </Button>
      </div>
      {groups.length > 0 && (
        <div className="form-row together-settings">
          <Field label="Group duration (s)" hint="How long each test in a group runs, on its own and together.">
            <input
              type="number"
              inputMode="decimal"
              min={0}
              step="any"
              value={dur}
              placeholder={n2(groupDefaultDuration(scale))}
              aria-invalid={durBad || undefined}
              onChange={(e) => setDur(e.target.value)}
            />
          </Field>
          <label className="check">
            <input type="checkbox" checked={baseline} onChange={(e) => setBaseline(e.target.checked)} />
            <span>Run each on its own first, for comparison</span>
          </label>
        </div>
      )}
    </section>
  );
}
