// Host diagnostics (the `pathgauge doctor` report): interfaces and checks,
// each check status shown as icon + label.
import { checkInfo } from '../lib/domain';
import type { CheckStatus, HostCheck, HostReport } from '../lib/types';
import { Badge, DataTable, Empty, KV } from './ui';

export function CheckBadge({ status }: { status: CheckStatus | string }) {
  const c = checkInfo[status as CheckStatus] ?? { kind: 'neutral' as const, icon: '•', label: status };
  return <Badge kind={c.kind} icon={c.icon} label={c.label} />;
}

export function checkCounts(checks: HostCheck[] | null | undefined): Record<string, number> {
  const out: Record<string, number> = { ok: 0, warn: 0, missing: 0, info: 0 };
  for (const c of checks ?? []) out[c.status] = (out[c.status] ?? 0) + 1;
  return out;
}

export function HostSummaryLine({ h }: { h: HostReport }) {
  const n = checkCounts(h.checks);
  return (
    <p className="muted">
      {h.os || 'unknown OS'}, kernel {h.kernel || '?'}, {h.arch}, {h.cpus} CPUs{h.virtualization ? `, ${h.virtualization}` : ''} · checks: {n.ok} ok,{' '}
      {n.warn} warnings, {n.missing} missing
    </p>
  );
}

export function HostDiagnostics({ host, onlyProblems }: { host: HostReport | null | undefined; onlyProblems?: boolean }) {
  if (!host) return <Empty>The agent has not reported host diagnostics yet.</Empty>;
  const n = checkCounts(host.checks);
  const checks = (host.checks ?? []).filter((c) => !onlyProblems || c.status === 'warn' || c.status === 'missing');
  return (
    <div className="host-diag">
      <div className="grid-2 tight">
        <KV
          rows={[
            ['Hostname', host.hostname || '—'],
            ['Operating system', host.os || '—'],
            ['Kernel', host.kernel || '—'],
            ['Architecture', `${host.arch || '?'} · ${host.cpus} CPUs`],
            ['Virtualisation', host.virtualization || 'none detected'],
          ]}
        />
        <div className="check-counts" aria-label="Check summary">
          <CheckCount status="ok" n={n.ok} />
          <CheckCount status="warn" n={n.warn} />
          <CheckCount status="missing" n={n.missing} />
          <CheckCount status="info" n={n.info} />
        </div>
      </div>
      {!onlyProblems && (
        <>
          <h3 className="subhead">Interfaces</h3>
          {(host.interfaces ?? []).length === 0 ? (
            <Empty>No interfaces reported.</Empty>
          ) : (
            <DataTable
              head={['Interface', 'State', 'MTU', 'Speed', 'Driver', 'Type']}
              numeric={[2, 3]}
              rows={(host.interfaces ?? []).map((i) => [
                <code>{i.name}</code>,
                i.oper_state === 'up' ? (
                  <Badge kind="good" icon="▲" label="up" />
                ) : i.oper_state === 'down' ? (
                  <Badge kind="neutral" icon="▼" label="down" />
                ) : (
                  <Badge kind="neutral" icon="?" label={i.oper_state || 'unknown'} />
                ),
                i.mtu,
                i.speed_mbps > 0 ? `${i.speed_mbps} Mbit/s` : '—',
                i.driver || '—',
                [i.wireless ? 'wireless' : 'wired', i.virtual ? 'virtual' : ''].filter(Boolean).join(', '),
              ])}
            />
          )}
        </>
      )}
      <h3 className="subhead">{onlyProblems ? 'Checks needing attention' : 'Checks'}</h3>
      {checks.length === 0 ? (
        <Empty>{onlyProblems ? 'No warnings.' : 'No checks reported.'}</Empty>
      ) : (
        <DataTable
          head={['Area', 'Check', 'Status', 'Detail', 'Impact / fix']}
          rows={checks.map((c) => [
            c.section,
            c.name,
            <CheckBadge status={c.status} />,
            c.detail,
            c.impact || c.fix ? (
              <>
                {c.impact && <div>{c.impact}</div>}
                {c.fix && <div className="cell-sub">Fix: {c.fix}</div>}
              </>
            ) : (
              <span className="muted">—</span>
            ),
          ])}
        />
      )}
    </div>
  );
}

function CheckCount({ status, n }: { status: CheckStatus; n: number }) {
  const c = checkInfo[status];
  return (
    <div className={`check-count badge-${c.kind}`}>
      <span aria-hidden="true">{c.icon}</span>
      <strong>{n}</strong>
      <span>{c.label}</span>
    </div>
  );
}
