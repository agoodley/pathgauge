import { useState, type FormEvent, type MouseEvent } from 'react';
import { api, errorMessage, reportURL } from '../lib/api';
import { useCan } from '../lib/auth';
import { formatCDP } from '../lib/cdp';
import { loadVal, phaseText } from '../lib/groups';
import { gradeLabel, runStatusInfo, severity } from '../lib/domain';
import { between, f0, f2, fmtDuration, fmtMbps, fmtMs, fmtPct, fmtTime } from '../lib/format';
import { Link, navigate } from '../lib/router';
import type { Finding, Report, Run } from '../lib/types';
import { Badge, Button, Card, CodeLine, ConfirmButton, DataTable, Empty, ErrorBox, Field, KV, LinkButton, Modal, Notice, PageHeader, ScoreBadge, Tile, toast } from '../components/ui';
import { StepSection } from '../components/StepResult';
import { profileLabel } from '../components/shared';

const WEIGHTS: Record<string, number> = { throughput: 25, latency: 20, loss: 20, bufferbloat: 20, path: 10, integrity: 5 };
const PART_ORDER = ['throughput', 'latency', 'loss', 'bufferbloat', 'path', 'integrity'];

export function RunResult({ run }: { run: Run }) {
  const rep = run.report ?? null;
  const isOperator = useCan('operator');
  const [share, setShare] = useState(false);

  const del = async () => {
    try {
      await api.del(`/api/runs/${encodeURIComponent(run.id)}`);
      toast('Run deleted');
      navigate('/reports');
    } catch (e) {
      toast(errorMessage(e), 'critical');
    }
  };

  const again = () => {
    const o = run.options ?? {};
    const q = new URLSearchParams({ agent: run.agent_id });
    if (o.tests?.length) q.set('tests', o.tests.join(','));
    else if (o.profile) q.set('profile', o.profile);
    if (o.mix) q.set('mix', o.mix);
    if (o.no_dscp) q.set('dscp', '0');
    if (o.cdp && formatCDP(o.cdp)) q.set('cdp', formatCDP(o.cdp));
    if (o.group_duration_s) q.set('gdur', String(o.group_duration_s));
    if (o.no_baseline) q.set('baseline', '0');
    navigate(`/runs/new?${q.toString()}`);
  };

  const st = runStatusInfo(run.status);
  const title = rep ? `${rep.client?.name || run.agent_name || 'client'} ↔ ${rep.endpoint?.name || rep.endpoint?.address || 'server'}` : `Run on ${run.agent_name || run.agent_id}`;
  const dur = rep ? between(rep.started_at, rep.ended_at) : between(run.started_at || run.created_at, run.ended_at);

  return (
    <div className="page">
      <PageHeader
        title={title}
        sub={
          <span className="inline-list">
            <Badge kind={st.kind} icon={st.icon} label={st.label} />
            <span>{fmtTime(rep?.started_at || run.started_at || run.created_at)}</span>
            <span>profile: {profileLabel(run)}</span>
            {dur !== null && <span>{fmtDuration(dur)}</span>}
            {run.agent_name && (
              <span>
                agent <Link to={`/agents/${run.agent_id}`}>{run.agent_name}</Link>
              </span>
            )}
            <span>{run.trigger === 'manual' ? `started by ${run.requested_by}` : run.trigger}</span>
          </span>
        }
        actions={rep ? <ScoreBadge score={rep.summary?.health_score} large /> : undefined}
      />

      <div className="action-bar">
        {rep && (
          <>
            <LinkButton href={reportURL(run.id, 'technical')} newTab>
              Technical report ↗
            </LinkButton>
            <LinkButton href={reportURL(run.id, 'executive')} newTab>
              Executive report ↗
            </LinkButton>
            <LinkButton href={reportURL(run.id, 'technical', true)} download>
              Download HTML
            </LinkButton>
            {isOperator && <Button onClick={() => setShare(true)}>Share link…</Button>}
            <Link className="btn" to={`/compare?runs=${run.id}`}>
              Compare with…
            </Link>
          </>
        )}
        {isOperator && run.agent_name && <Button onClick={again}>Run again</Button>}
        {isOperator && <ConfirmButton label="Delete run" confirmLabel="Delete" prompt="Delete this run and its results?" onConfirm={del} small={false} />}
      </div>

      {(run.status === 'failed' || run.status === 'cancelled') && (
        <Notice kind={run.status === 'failed' ? 'critical' : 'neutral'}>
          This run {run.status === 'failed' ? 'failed' : 'was cancelled'}
          {run.error ? `: ${run.error.replace(/[.\s]*$/, '')}.` : '.'} {!rep && 'No results were stored.'}
        </Notice>
      )}

      {rep ? <ReportView rep={rep} profile={profileLabel(run)} /> : run.status === 'done' && <Empty>This run has no stored results.</Empty>}

      {share && <ShareDialog runId={run.id} onClose={() => setShare(false)} />}
    </div>
  );
}

function scrollToStep(e: MouseEvent, id: string) {
  e.preventDefault();
  const el = document.getElementById(`step-${id}`);
  if (!el) return;
  el.scrollIntoView({ behavior: 'smooth', block: 'start' });
  el.classList.add('flash');
  window.setTimeout(() => el.classList.remove('flash'), 1600);
}

function ReportView({ rep, profile }: { rep: Report; profile: string }) {
  const s = rep.summary;
  const steps = rep.steps ?? [];
  const findings = [...(rep.findings ?? [])].sort((a, b) => severity(a.severity).rank - severity(b.severity).rank);
  const parts = PART_ORDER.filter((k) => s.score_parts && k in s.score_parts);
  const fp = Object.entries(s.fingerprint ?? {}).sort((a, b) => a[0].localeCompare(b[0]));
  const info = steps.find((x) => x.test === 'info' && x.status === 'ok')?.result as
    | { client_clock?: { synced: boolean; est_error_ms: number }; endpoint_clock?: { synced: boolean; est_error_ms: number }; owd_trustworthy?: boolean }
    | undefined;

  return (
    <>
      <div className="tiles">
        {s.down_mbps > 0 && <Tile label="Download" value={fmtMbps(s.down_mbps)} sub={s.down_single_mbps > 0 ? `single stream ${fmtMbps(s.down_single_mbps)}` : 'parallel streams'} />}
        {s.up_mbps > 0 && <Tile label="Upload" value={fmtMbps(s.up_mbps)} sub={s.up_single_mbps > 0 ? `single stream ${fmtMbps(s.up_single_mbps)}` : 'parallel streams'} />}
        {s.idle_rtt_ms > 0 && <Tile label="Idle RTT" value={fmtMs(s.idle_rtt_ms)} sub={`jitter ${fmtMs(s.jitter_ms)} · loss ${fmtPct(s.loss_pct)}`} />}
        {s.bloat_grade && (
          <Tile
            label="Bufferbloat"
            value={s.bloat_grade}
            status={<Badge kind={gradeLabel(s.bloat_grade).kind} icon={gradeLabel(s.bloat_grade).icon} label={gradeLabel(s.bloat_grade).label} />}
            sub={`${f0(s.rpm)} RPM responsiveness`}
          />
        )}
        {s.mos > 0 && <Tile label="Voice (MOS)" value={f2(s.mos)} sub="simulated G.711 call" />}
        {s.path_mtu > 0 && <Tile label="Path MTU" value={String(s.path_mtu)} sub={`${s.hops_up} hops out · ${s.hops_down} back${s.nat ? ' · NAT' : ''}`} />}
      </div>

      <div className="grid-2">
        <Card title="Health score">
          <div className="score-row">
            <ScoreBadge score={s.health_score} large />
            <p className="muted small">The score combines throughput, latency, loss, bufferbloat, path and integrity. Each part scores up to its weight.</p>
          </div>
          {parts.length > 0 && (
            <table className="data parts">
              <thead>
                <tr>
                  <th scope="col">Part</th>
                  <th scope="col" className="num">
                    Score
                  </th>
                  <th scope="col" aria-hidden="true" />
                </tr>
              </thead>
              <tbody>
                {parts.map((k) => {
                  const v = s.score_parts?.[k] ?? 0;
                  const w = WEIGHTS[k] ?? 100;
                  return (
                    <tr key={k}>
                      <th scope="row">{k[0].toUpperCase() + k.slice(1)}</th>
                      <td className="num">
                        {f0(v)} / {w}
                      </td>
                      <td className="meter-cell">
                        <div className="meter" aria-hidden="true">
                          <div style={{ width: `${Math.max(0, Math.min(100, (100 * v) / w))}%` }} />
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </Card>
        <Card title="Link fingerprint">
          {fp.length ? <KV rows={fp.map(([k, v]) => [k, v])} /> : <Empty>No fingerprint for this run.</Empty>}
        </Card>
      </div>

      <Card title={`Findings (${findings.length})`}>
        {findings.length === 0 ? <Empty>No findings.</Empty> : findings.map((f, i) => <FindingCard key={f.id + i} f={f} />)}
      </Card>

      <LoadSection rep={rep} />

      <h2 className="section-title">Results by step</h2>
      {steps.length === 0 && <Empty>No steps recorded.</Empty>}
      <nav className="step-index" aria-label="Steps">
        {steps.map((x) => (
          <a key={x.id} href={`#step-${x.id}`} onClick={(e) => scrollToStep(e, x.id)} className={x.status !== 'ok' ? 'step-index-err' : undefined}>
            {x.status !== 'ok' && <span aria-hidden="true">✖ </span>}
            {x.title}
            {x.phase && <span className="step-index-phase"> · {x.phase === 'alone' ? 'on its own' : 'together'}</span>}
          </a>
        ))}
      </nav>
      {steps.map((x) => (
        <StepSection key={x.id} step={x} phase={phaseText(rep, x)} />
      ))}

      <Card title="Methodology">
        <KV
          rows={[
            ['Software', rep.software],
            ['Profile', profile || rep.profile],
            ['Run ID', <code>{rep.id}</code>],
            ['Started', fmtTime(rep.started_at)],
            ['Client', `${rep.client?.name ?? ''} ${rep.client?.local_addr ?? ''} (seen by endpoint as ${rep.client?.observed_addr || '?'})`],
            ['Endpoint', `${rep.endpoint?.name ?? ''} ${rep.endpoint?.address ?? ''} ${rep.endpoint?.software ?? ''}`],
            ...(info
              ? ([
                  [
                    'Clocks',
                    `client synced=${info.client_clock?.synced} (±${f2(info.client_clock?.est_error_ms)} ms), endpoint synced=${info.endpoint_clock?.synced} (±${f2(info.endpoint_clock?.est_error_ms)} ms); one-way delay reported: ${info.owd_trustworthy}`,
                  ],
                ] as [string, string][])
              : []),
            ...(rep.notes ?? []).map((n) => ['Note', n] as [string, string]),
          ]}
        />
      </Card>
    </>
  );
}

/**
 * Tests run together (groups): each key result on its own and together,
 * as loadSection in internal/report/mix.go. No chart: the measures have
 * different units, so it would be one small chart per measure with two
 * bars, which says less than the table.
 */
function LoadSection({ rep }: { rep: Report }) {
  const groups = rep.groups ?? [];
  if (!groups.length) return null;
  const cmp = rep.load_comparisons ?? [];
  const steps = rep.steps ?? [];
  // The step of a grouped test in a phase, to link its value to.
  const stepOf = (group: string, phase: string, test: string) =>
    steps.find((s) => s.group === group && s.phase === phase && (s.id.endsWith(`-${test}`) || s.id.endsWith(`-${test}-fwd`)));
  const link = (group: string, phase: string, test: string, text: string) => {
    const s = stepOf(group, phase, test);
    return s ? (
      <a href={`#step-${s.id}`} onClick={(e) => scrollToStep(e, s.id)}>
        {text}
      </a>
    ) : (
      text
    );
  };
  return (
    <Card title="Under load: tests run together" id="under-load">
      {groups.map((g) => {
        const rows = cmp.filter((c) => c.group === g.id);
        const compared = new Set(rows.map((c) => c.test));
        const missing = g.baseline ? g.tests.filter((t) => !compared.has(t)) : [];
        return (
          <div key={g.id} className="load-group">
            {groups.length > 1 && <h3>Group {g.id.replace(/^g/, '')}</h3>}
            <p>
              {g.tests.map((t, i) => (
                <span key={t}>
                  {i > 0 && ' + '}
                  <code>{t}</code>
                </span>
              ))}{' '}
              ran at the same time, each for {f0(g.duration_s)} s.{' '}
              {g.baseline ? 'Each first ran on its own for the same time.' : 'They didn’t run on their own first, so there is nothing to compare with.'}
            </p>
            {rows.length > 0 && (
              <div className="load-table">
                <DataTable
                  head={['Test', 'Measure', 'On its own', 'Together', 'Change']}
                  numeric={[2, 3]}
                  rows={rows.map((c) => [
                    <>
                      {c.title} <code className="small muted">{c.test}</code>
                    </>,
                    c.metric,
                    link(g.id, 'alone', c.test, loadVal(c.alone, c.unit)),
                    link(g.id, 'together', c.test, loadVal(c.together, c.unit)),
                    c.worse ? <Badge kind="warning" icon="▲" label="Worse" /> : <Badge kind="good" icon="✔" label="Similar" />,
                  ])}
                />
              </div>
            )}
            {missing.length > 0 && (
              <p className="muted small">
                Not compared: {missing.join(', ')} (no key result to compare, or a step didn’t finish).
              </p>
            )}
          </div>
        );
      })}
    </Card>
  );
}

function FindingCard({ f }: { f: Finding }) {
  const sv = severity(f.severity);
  return (
    <article className={`finding finding-${sv.kind}`}>
      <div className="finding-sev">
        <Badge kind={sv.kind} icon={sv.icon} label={sv.label} />
        {f.confidence && <span className="cell-sub">{f.confidence} confidence</span>}
      </div>
      <div className="finding-body">
        <h3>{f.title}</h3>
        <p>{f.detail}</p>
        {f.recommendation && (
          <p className="rec">
            <strong>Recommendation:</strong> {f.recommendation}
          </p>
        )}
        {(f.evidence ?? []).length > 0 && (
          <p className="evidence">
            Evidence:{' '}
            {(f.evidence ?? []).map((id) => (
              <a key={id} href={`#step-${id}`} onClick={(e) => scrollToStep(e, id)}>
                {id}
              </a>
            ))}
          </p>
        )}
      </div>
    </article>
  );
}

function ShareDialog({ runId, onClose }: { runId: string; onClose: () => void }) {
  const [hours, setHours] = useState('72');
  const [template, setTemplate] = useState<'technical' | 'executive'>('executive');
  const [url, setUrl] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const r = await api.post<{ url: string }>(`/api/runs/${encodeURIComponent(runId)}/share`, { hours: Number(hours) || 72, template });
      setUrl(r.url);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title="Create a share link" open onClose={onClose}>
      {!url ? (
        <form onSubmit={create} className="form-grid">
          <p className="muted">Anyone with the link can view this run's report, without signing in, until it expires.</p>
          <Field label="Report">
            <select value={template} onChange={(e) => setTemplate(e.target.value as 'technical' | 'executive')}>
              <option value="executive">Executive summary</option>
              <option value="technical">Technical diagnostic</option>
            </select>
          </Field>
          <Field label="Expires after (hours)" hint="Up to 2160 hours (90 days).">
            <input type="number" min={1} max={2160} value={hours} onChange={(e) => setHours(e.target.value)} />
          </Field>
          <ErrorBox error={error} />
          <div className="form-actions">
            <Button variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" busy={busy}>
              Create link
            </Button>
          </div>
        </form>
      ) : (
        <>
          <Notice kind="good">Share link created. It expires in {hours} hours.</Notice>
          <CodeLine text={url} />
          <div className="form-actions">
            <a className="btn" href={url} target="_blank" rel="noopener">
              Open ↗
            </a>
            <Button variant="primary" onClick={onClose}>
              Done
            </Button>
          </div>
        </>
      )}
    </Modal>
  );
}
