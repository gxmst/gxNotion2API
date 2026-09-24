import { expect, test, type Page } from '@playwright/test';

const email = 'owner@example.com';

async function mockPanels(page: Page, options: { holdQuickTest?: boolean } = {}) {
  page.on('pageerror', (error) => { throw error; });
  const accountSaves: Record<string, unknown>[] = [];
  const quickTests: Record<string, unknown>[] = [];
  const settingsSaves: Record<string, unknown>[] = [];
  let configVersion = 0;
  let releaseQuickTest = () => {};
  const quickTestGate = new Promise<void>((resolve) => { releaseQuickTest = resolve; });
  const config = () => ({
    success: true,
    config: {
      host: '0.0.0.0', port: 8787, default_model: 'auto', dispatch: { account_max_concurrency: 1 + configVersion },
      features: { use_web_search: false, is_custom_agent: false },
      accounts: [{ email, cooldown_until: 'stale-runtime-state' }],
    },
    models: [{ id: 'auto', name: 'Auto' }, { id: 'model-a', name: 'Model A' }],
  });
  const accounts = {
    active_account: email, active_workspace_id: 'ws-1',
    items: [{ email, disabled: false, workspaces: [
      { id: 'ws-1', name: '工作区一', priority: 1, hourly_quota: 10, max_concurrency: 2, eligible: true, default: true, model_capabilities: { mode: 'manual', models: [{ id: 'model-a', name: 'Model A', enabled: true }] } },
      { id: 'ws-2', name: '工作区二', priority: 5, hourly_quota: 0, max_concurrency: 1, eligible: true, model_capabilities: { mode: 'auto_only', models: [] } },
    ] }],
  };
  const responses: Record<string, () => unknown> = {
    '/admin/verify': () => ({ authenticated: true, password_configured: true, admin_enabled: true }),
    '/admin/config': config,
    '/admin/version': () => ({ version: 'test', default_model: 'auto', model_count: 2 }),
    '/healthz': () => ({ ok: true, session_ready: true }),
    '/admin/accounts/ai-usage': () => ({ accounts: [] }),
    '/admin/conversations': () => ({ items: [] }),
  };
  await page.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === '/admin/accounts') {
      if (request.method() === 'PUT') accountSaves.push(request.postDataJSON());
      return route.fulfill({ json: accounts });
    }
    if (url.pathname === '/admin/accounts/test') {
      quickTests.push(request.postDataJSON());
      if (options.holdQuickTest) await quickTestGate;
      return route.fulfill({ json: { success: true, text: 'NOTION2API_ACCOUNT_OK' } });
    }
    if (url.pathname === '/admin/settings') {
      settingsSaves.push(request.postDataJSON());
      return route.fulfill({ json: { success: true } });
    }
    if (url.pathname in responses) return route.fulfill({ json: responses[url.pathname]() });
    if (url.pathname === '/admin/events') return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    return route.continue();
  });
  return { accountSaves, quickTests, settingsSaves, releaseQuickTest, bumpConfig: () => { configVersion += 1; } };
}

async function openTab(page: Page, name: string) {
  await page.goto('/admin');
  await page.getByRole('button', { name: '管理控制台' }).click();
  await page.getByRole('button', { name, exact: true }).click();
}

test.beforeEach(({ page }) => {
  test.skip(page.viewportSize()!.width < 1024, 'desktop layout only');
});

test('workspace scheduling edits are kept per workspace and reach the save payload', async ({ page }) => {
  const { accountSaves } = await mockPanels(page);
  await openTab(page, '账号');
  const priority = page.locator('label:text-is("Priority")').locator('xpath=../..').locator('input');
  await expect(priority).toHaveValue('1');
  await priority.fill('7');
  await expect(priority).toHaveValue('7');
  const concurrency = page.locator('label:text-is("Max Concurrency")').locator('xpath=../..').locator('input');
  await concurrency.fill('4');
  await page.getByRole('switch', { name: '禁用账号' }).click();
  await expect(page.getByRole('switch', { name: '禁用账号' })).toBeChecked();
  await page.getByRole('button', { name: '保存工作区设置' }).click();
  await expect.poll(() => accountSaves.length).toBe(1);
  expect(accountSaves[0]).toEqual({ email, workspace_id: 'ws-1', priority: 7, hourly_quota: 10, max_concurrency: 4, disabled: true });
});

test('quick test is single-flight and the sidebar follows the tested workspace', async ({ page }) => {
  const { quickTests, releaseQuickTest } = await mockPanels(page, { holdQuickTest: true });
  await openTab(page, '账号');
  const details = page.getByRole('combobox').filter({ hasText: '工作区一' }).first();
  await details.click();
  await page.getByRole('option', { name: /工作区二/ }).click();
  const testWorkspace = page.getByRole('button', { name: '测试工作区' });
  await testWorkspace.click();
  await expect(page.getByRole('button', { name: '测试中...' }).first()).toBeDisabled();
  await expect.poll(() => quickTests.length).toBe(1);
  expect(quickTests[0]).toMatchObject({ email, workspace_id: 'ws-2', model: 'auto' });
  await expect(page.getByText('测试中：owner@example.com · 工作区二')).toBeVisible();
  releaseQuickTest();
  await expect(page.getByText('测试成功：owner@example.com · 工作区二')).toBeVisible();
  expect(quickTests).toHaveLength(1);
});

test('unsaved settings survive a resync, guard tab changes, and save without accounts', async ({ page }) => {
  const { settingsSaves, bumpConfig } = await mockPanels(page);
  await openTab(page, '设置');
  await expect(page.getByText('保存后立即热更新。')).toBeVisible();
  const host = page.locator('label:text-is("监听 Host")').locator('xpath=ancestor::div[2]').locator('input').first();
  await host.fill('127.0.0.1');
  await expect(page.getByText('有未保存的修改')).toBeVisible();
  bumpConfig();
  await page.getByRole('button', { name: '重新同步' }).click();
  await expect(page.getByText('后台配置已更新')).toBeVisible();
  await expect(host).toHaveValue('127.0.0.1');
  page.once('dialog', (dialog) => void dialog.dismiss());
  await page.getByRole('button', { name: '账号', exact: true }).click();
  await expect(host).toHaveValue('127.0.0.1');
  await page.getByRole('button', { name: '保存设置' }).click();
  await expect.poll(() => settingsSaves.length).toBe(1);
  const saved = (settingsSaves[0] as { config: Record<string, unknown> }).config;
  expect(saved.host).toBe('127.0.0.1');
  expect(saved.dispatch).toEqual({ account_max_concurrency: 2 });
  expect(saved).not.toHaveProperty('accounts');
  expect(saved.features).not.toHaveProperty('is_custom_agent_builder');
  expect(saved.features).not.toHaveProperty('use_custom_agent_draft');
  await expect(page.getByText('有未保存的修改')).toHaveCount(0);
  await page.getByRole('button', { name: '账号', exact: true }).click();
  await expect(page.getByRole('button', { name: '保存工作区设置' })).toBeVisible();
});
