import { useApi, useDocumentTitle, useInterval } from '../lib/hooks';
import { useLiveRefresh } from '../lib/live';
import { Link } from '../lib/router';
import type { Run } from '../lib/types';
import { ErrorBox, Loading, PageHeader } from '../components/ui';
import { LiveRun } from './LiveRun';
import { RunResult } from './RunResult';

/** /runs/:id — the live view while a run is active, the result afterwards. */
export function RunPage({ id }: { id: string }) {
  const run = useApi<Run>(`/api/runs/${encodeURIComponent(id)}`);
  const active = run.data?.status === 'running' || run.data?.status === 'queued';
  useDocumentTitle(run.data ? `Run ${id.slice(0, 8)}${active ? ' (running)' : ''}` : 'Run');
  useLiveRefresh((m) => m.type === 'run_status' && m.run_id === id, run.reload, 250);
  // Fallback in case a status message is missed while disconnected.
  useInterval(run.reload, 10000, active);

  if (run.error && !run.data) {
    return (
      <div className="page">
        <PageHeader title="Run" />
        <ErrorBox error={run.error} onRetry={run.reload} />
        <Link to="/reports">Back to runs</Link>
      </div>
    );
  }
  if (!run.data)
    return (
      <div className="page">
        <Loading />
      </div>
    );
  if (active) return <LiveRun run={run.data} onChange={run.reload} />;
  return <RunResult run={run.data} />;
}
