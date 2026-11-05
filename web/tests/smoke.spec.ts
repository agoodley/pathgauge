// Browser smoke test: sign in, visit every page, open a run result,
// compare runs, start a short custom run and watch it finish, check dark
// mode, and fail on any console error. Screenshots go to $SHOTS.
import { expect, request, test, type Page } from '@playwright/test';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const USER = process.env.PATHGAUGE_USER ?? 'admin';
const PASSWORD = process.env.PATHGAUGE_PASSWORD ?? 'admin-password-123';
const SHOTS = process.env.SHOTS ?? 'test-results/shots';
const RUN_TIMEOUT = Number(process.env.RUN_TIMEOUT_MS ?? 240_000);

interface AgentJSON {
  id: string;
  name: string;
  online: boolean;
  busy: boolean;
  revoked: boolean;
}
interface RunJSON {
  id: string;
  status: string;
}

async function shot(page: Page, name: string) {
  mkdirSync(SHOTS, { recursive: true });
  await page.screenshot({ path: join(SHOTS, `${name}.png`), fullPage: true });
}

/** Wait for data to load: no spinners, no error boxes. */
async function settled(page: Page) {
  await expect(page.locator('h1').first()).toBeVisible();
  await expect(page.locator('.loading')).toHaveCount(0, { timeout: 15_000 });
  await expect(page.locator('main .alert-critical[role=alert]')).toHaveCount(0);
  await page.waitForTimeout(400); // let charts render
}

test('dashboard smoke test', async ({ page }) => {
  const problems: string[] = [];
  page.on('console', (m) => {
    if (m.type() === 'error') problems.push(`console error on ${page.url()}: ${m.text()}`);
  });
  page.on('pageerror', (e) => problems.push(`page error on ${page.url()}: ${e.message}`));

  await test.step('sign in', async () => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await shot(page, '00-login');
    await page.getByLabel('Username').fill(USER);
    await page.getByLabel('Password').fill(PASSWORD);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByRole('navigation', { name: 'Main navigation' }).or(page.locator('aside.sidebar'))).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible();
  });

  const agents = (await (await page.request.get('/api/agents')).json()) as AgentJSON[];
  const agent = agents.find((a) => !a.revoked);
  expect(agent, 'the server needs at least one enrolled agent').toBeTruthy();
  const done = (await (await page.request.get('/api/runs?status=done&limit=20')).json()) as RunJSON[];

  await test.step('visit every page', async () => {
    const pages: [string, string][] = [
      ['/', '01-overview'],
      ['/agents', '02-agents'],
      [`/agents/${agent!.id}`, '03-agent-detail'],
      [`/runs/new?agent=${agent!.id}`, '04-new-run'],
      [`/monitoring?agent=${agent!.id}`, '05-monitoring'],
      ['/reports', '06-reports'],
      ['/compare', '07-compare-pick'],
      ['/schedules', '08-schedules'],
      ['/alerts', '09-alerts'],
      ['/admin/users', '10-admin-users'],
      ['/admin/tokens', '11-admin-tokens'],
      ['/admin/settings', '12-admin-settings'],
      ['/admin/audit', '13-admin-audit'],
    ];
    for (const [path, name] of pages) {
      await page.goto(path);
      await settled(page);
      await shot(page, name);
    }
    // Client-side navigation through the sidebar, too.
    await page.getByRole('link', { name: 'Overview', exact: true }).click();
    await expect(page).toHaveURL(/\/$/);
    await page.getByRole('link', { name: 'Alerts', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Alerts' })).toBeVisible();
  });

  await test.step('open a run result', async () => {
    if (done.length === 0) return test.info().annotations.push({ type: 'skipped', description: 'no completed runs' });
    await page.goto(`/runs/${done[0].id}`);
    await settled(page);
    await expect(page.getByRole('heading', { name: /^Findings/ })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Results by step' })).toBeVisible();
    await expect(page.locator('section.step').first()).toBeVisible();
    await expect(page.getByRole('link', { name: /Technical report/ })).toHaveAttribute('href', /report\.html\?template=technical/);
    await shot(page, '14-run-result');
    const report = await page.request.get(`/api/runs/${done[0].id}/report.html?template=executive`);
    expect(report.ok()).toBeTruthy();
  });

  await test.step('compare two runs', async () => {
    if (done.length < 2) return test.info().annotations.push({ type: 'skipped', description: 'compare needs two completed runs' });
    await page.goto(`/compare?runs=${done[1].id},${done[0].id}`);
    await settled(page);
    await expect(page.getByRole('heading', { name: 'Key metrics' })).toBeVisible();
    await shot(page, '15-compare');
  });

  await test.step('traffic mix options and results', async () => {
    // The mix chooser appears with the mix test; custom counts are validated.
    await page.goto(`/runs/new?agent=${agent!.id}&tests=info,mix,capacity-voice`);
    await settled(page);
    await expect(page.getByRole('heading', { name: '3. Traffic mix and calls' })).toBeVisible();
    await expect(page.getByRole('radio', { name: /^Office/ })).toBeChecked();
    await expect(page.getByLabel('Mark calls with DSCP (EF voice, AF41 video)')).toBeChecked();
    await page.getByRole('radio', { name: /^Custom Choose how many/ }).check();
    // Custom starts from the selected scenario (office: 10 voice calls).
    const voice = page.getByRole('spinbutton', { name: 'Voice call (G.711)', exact: true });
    await expect(voice).toHaveValue('10');
    await voice.fill('300');
    await expect(page.getByText('Voice call (G.711): at most 256 sessions.')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Start run' })).toBeDisabled();
    await voice.fill('4');
    await expect(page.locator('p.mix-summary')).toContainText('4 G.711 voice calls');
    await shot(page, '24-new-run-mix');

    // Render a finished mix or capacity run if the server has one.
    const recent = (await (await page.request.get('/api/runs?status=done&limit=50')).json()) as (RunJSON & { options?: { tests?: string[] } })[];
    const mixRun = recent.find((r) => (r.options?.tests ?? []).some((t) => t === 'mix' || t.startsWith('capacity-')));
    if (!mixRun) return test.info().annotations.push({ type: 'skipped', description: 'no completed mix or capacity run' });
    await page.goto(`/runs/${mixRun.id}`);
    await settled(page);
    if ((mixRun.options?.tests ?? []).includes('mix')) {
      await expect(page.getByText('Sessions with good quality, by kind')).toBeVisible();
      await expect(page.getByText(/^Every session/).first()).toBeVisible();
    }
    if ((mixRun.options?.tests ?? []).some((t) => t.startsWith('capacity-'))) {
      await expect(page.getByRole('heading', { name: 'On their own' }).first()).toBeVisible();
    }
    await shot(page, '25-mix-result');
  });

  await test.step('create, use and delete things', async () => {
    const tag = `smoke-${Date.now()}`;
    const row = (text: string) => page.locator('tr', { hasText: text });
    const confirmDelete = async (text: string, label = 'Delete', confirm = 'Confirm') => {
      await row(text).getByRole('button', { name: label }).click();
      await row(text).getByRole('button', { name: confirm }).click();
      await expect(row(text)).toHaveCount(0);
    };

    // Schedule: create, edit, delete.
    await page.goto('/schedules');
    await settled(page);
    await page.getByRole('button', { name: 'New schedule' }).click();
    const dlg = page.getByRole('dialog');
    await dlg.getByLabel('Note').fill(tag);
    await dlg.getByRole('button', { name: 'Every 6 hours' }).click();
    await dlg.getByRole('button', { name: 'Create schedule' }).click();
    await expect(row(tag)).toContainText('every 6 hours');
    await row(tag).getByRole('button', { name: 'Edit' }).click();
    await page.getByRole('dialog').getByLabel(/Cron expression/).fill('15 4 * * 1-5');
    await page.getByRole('dialog').getByRole('button', { name: 'Save schedule' }).click();
    await expect(row(tag)).toContainText('weekdays at 04:15');
    await confirmDelete(tag);

    // Alert rule: create, send a test notification, delete.
    await page.goto('/alerts');
    await settled(page);
    await page.getByRole('button', { name: 'New rule' }).click();
    await page.getByRole('dialog').getByLabel('Name').fill(tag);
    await page.getByRole('dialog').getByRole('button', { name: 'Create rule' }).click();
    await expect(row(tag)).toContainText('RTT p95 > 50 ms for 5 min');
    await row(tag).getByRole('button', { name: 'Send test' }).click();
    await expect(page.getByText('Test notification sent via log')).toBeVisible();
    await confirmDelete(tag);

    // Join token: shown once with the join command, then deleted.
    await page.goto('/agents');
    await settled(page);
    await page.getByLabel('Note (optional)').fill(tag);
    await page.getByRole('button', { name: 'Create join token' }).click();
    await expect(page.getByText('Join token created.')).toBeVisible();
    await expect(page.locator('.codeline code', { hasText: 'agent join https://' })).toBeVisible();
    await shot(page, '23-join-token');
    await confirmDelete(tag);

    // Annotation on the agent page.
    await page.goto(`/agents/${agent!.id}`);
    await settled(page);
    await page.getByLabel('Text').fill(tag);
    await page.getByRole('button', { name: 'Add annotation' }).click();
    await expect(row(tag)).toBeVisible();
    await confirmDelete(tag);

    // API token: shown once, then revoked.
    await page.goto('/admin/tokens');
    await settled(page);
    await page.getByLabel('Name').fill(tag);
    await page.getByRole('button', { name: 'Create token' }).click();
    await expect(page.locator('.codeline code', { hasText: 'pgt_' })).toBeVisible();
    await confirmDelete(tag, 'Revoke', 'Revoke');

    if (done.length) {
      // Share link for a run's executive report.
      await page.goto(`/runs/${done[0].id}`);
      await settled(page);
      await page.getByRole('button', { name: /Share link/ }).click();
      await page.getByRole('dialog').getByRole('button', { name: 'Create link' }).click();
      const url = await page.getByRole('dialog').locator('.codeline code').textContent();
      expect(url).toContain('/share/');
      const shared = await page.request.get(new URL(url!).pathname);
      expect(shared.ok()).toBeTruthy();
      await page.getByRole('dialog').getByRole('button', { name: 'Done' }).click();

      // Import that run's results file, open it, then delete the copy.
      const full = (await (await page.request.get(`/api/runs/${done[0].id}`)).json()) as { report: unknown };
      mkdirSync('test-results', { recursive: true });
      const file = join('test-results', `${tag}.json`);
      writeFileSync(file, JSON.stringify(full.report));
      await page.goto('/reports');
      await settled(page);
      await page.getByLabel('Results file (.json)').setInputFiles(file);
      await page.getByRole('button', { name: 'Import', exact: true }).click();
      await expect(page.getByRole('heading', { name: 'Results by step' })).toBeVisible();
      await page.getByRole('button', { name: 'Delete run' }).click();
      await page.getByRole('button', { name: 'Delete', exact: true }).click();
      await expect(page).toHaveURL(/\/reports$/);
    }
  });

  await test.step('start a short run and watch it live', async () => {
    const a = (await (await page.request.get(`/api/agents/${agent!.id}`)).json()) as AgentJSON;
    expect(a.online && !a.busy, `${a.name} must be online and idle to start a run`).toBeTruthy();
    await page.goto(`/runs/new?agent=${a.id}`);
    await settled(page);
    await page.getByRole('radio', { name: 'Custom' }).click();
    // Custom starts with info, latency, tcp-down and tcp-up selected.
    await page.getByLabel(/^TCP upload, parallel streams/).uncheck();
    await page.getByLabel(/Scale/).fill('0.3');
    await expect(page.getByText(/3 tests/)).toBeVisible();
    await page.getByRole('button', { name: 'Start run' }).click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f]+$/);
    await expect(page.getByRole('heading', { name: /^Running:/ })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Steps' })).toBeVisible();
    // Catch the live throughput chart during the TCP test if we can.
    await page
      .getByText('Throughput at the receiver')
      .waitFor({ timeout: 60_000 })
      .then(() => page.waitForTimeout(1500))
      .catch(() => undefined);
    await shot(page, '16-live-run');
    await expect(page.getByRole('heading', { name: 'Results by step' })).toBeVisible({ timeout: RUN_TIMEOUT });
    await settled(page);
    await expect(page.locator('section.step')).toHaveCount(3);
    await shot(page, '17-live-run-finished');
  });

  await test.step('dark mode', async () => {
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.goto('/');
    await settled(page);
    await shot(page, '18-overview-dark');
    if (done.length) {
      await page.goto(`/runs/${done[0].id}`);
      await settled(page);
      await shot(page, '19-run-result-dark');
    }
    await page.goto(`/monitoring?agent=${agent!.id}`);
    await settled(page);
    await shot(page, '20-monitoring-dark');
  });

  await test.step('narrow screen', async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.emulateMedia({ colorScheme: 'light' });
    await page.goto('/');
    await settled(page);
    await page.getByRole('button', { name: 'Open menu' }).click();
    await expect(page.getByRole('link', { name: 'Monitoring', exact: true })).toBeVisible();
    await shot(page, '21-mobile-menu');
  });

  await test.step('sign out', async () => {
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  });

  expect(problems, problems.join('\n')).toEqual([]);
});

test('viewer gets a read-only dashboard', async ({ browser, baseURL }) => {
  // Create a throwaway viewer with the admin's API session.
  const admin = await request.newContext({ baseURL, ignoreHTTPSErrors: true, extraHTTPHeaders: { 'X-PathGauge': '1' } });
  expect((await admin.post('/api/auth/login', { data: { username: USER, password: PASSWORD } })).ok()).toBeTruthy();
  const name = `smoke-viewer-${Date.now()}`;
  const pw = 'viewer-password-123';
  const created = await admin.post('/api/users', { data: { username: name, password: pw, role: 'viewer' } });
  expect(created.ok(), await created.text()).toBeTruthy();
  const { id } = (await created.json()) as { id: number };

  const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  const problems: string[] = [];
  page.on('console', (m) => {
    if (m.type() === 'error') problems.push(`console error on ${page.url()}: ${m.text()}`);
  });
  page.on('pageerror', (e) => problems.push(`page error on ${page.url()}: ${e.message}`));
  try {
    await page.goto('/');
    await page.getByLabel('Username').fill(name);
    await page.getByLabel('Password').fill(pw);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible();
    // No operator/admin navigation or actions.
    await expect(page.getByRole('link', { name: 'New run', exact: true })).toHaveCount(0);
    await expect(page.getByRole('link', { name: 'Admin', exact: true })).toHaveCount(0);
    const agents = (await (await page.request.get('/api/agents')).json()) as AgentJSON[];
    const runs = (await (await page.request.get('/api/runs?status=done&limit=1')).json()) as RunJSON[];
    const paths = ['/', '/agents', '/monitoring', '/reports', '/compare', '/schedules', '/alerts'];
    if (agents[0]) paths.push(`/agents/${agents[0].id}`);
    if (runs[0]) paths.push(`/runs/${runs[0].id}`);
    for (const p of paths) {
      await page.goto(p);
      await settled(page);
    }
    if (runs[0]) {
      await expect(page.getByRole('button', { name: 'Delete run' })).toHaveCount(0);
      await expect(page.getByRole('button', { name: /Share link/ })).toHaveCount(0);
      await expect(page.getByRole('link', { name: /Technical report/ })).toBeVisible();
    }
    if (agents[0]) {
      await page.goto(`/agents/${agents[0].id}`);
      await settled(page);
      await expect(page.getByRole('button', { name: 'Save settings' })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Revoke agent' })).toHaveCount(0);
      await shot(page, '22-viewer-agent-detail');
    }
    await page.goto('/admin/users');
    await expect(page.getByText('Administration needs the admin role.')).toBeVisible();
  } finally {
    await ctx.close();
    await admin.delete(`/api/users/${id}`);
    await admin.dispose();
  }
  expect(problems, problems.join('\n')).toEqual([]);
});
