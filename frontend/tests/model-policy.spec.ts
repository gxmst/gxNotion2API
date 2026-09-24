import { expect, test, type Page } from '@playwright/test';
import type { ModelPolicyEdit, ModelPolicyRestorePoint, ModelPolicySnapshot } from '../lib/services/admin/types';

interface MockOptions {
  role?: string;
  error?: number;
  // Apply the write upstream but lose the reply (simulates a gateway error after success).
  errorAfterWrite?: boolean;
  locked?: boolean;
  noneAllowed?: boolean;
}

async function mockModelPolicy(page: Page, options: MockOptions = {}) {
  page.on('pageerror', (error) => { throw error; });
  const writes: ModelPolicyEdit[] = [];
  const reads: string[] = [];
  const capabilityRefreshes: Array<{ email: string; workspace_id: string }> = [];
  let accountReads = 0;
  const initial: ModelPolicySnapshot = {
    email: 'owner@example.com', workspace_id: 'policy-space', scope: 'personal',
    membership_type: options.role || 'owner', can_edit: !options.role || options.role === 'owner',
    policy: { disabledModels: ['retired-model'], disabledProviders: [] }, policy_present: true, revision: 'initial',
    models: [
      { id: 'model-a', name: 'Model A', provider: 'glm', available: true, allowed: true },
      { id: 'model-b', name: 'Model B', provider: 'anthropic', available: true, allowed: true },
      { id: 'paid-only', name: 'Paid Only', provider: 'openai', available: false, allowed: false, disabled_reason: 'trial_not_allowed' },
    ],
  };
  if (options.locked) {
    initial.policy = { disabledModels: ['model-b', 'paid-only', 'retired-model'], disabledProviders: ['anthropic', 'openai'] };
    initial.models = initial.models.map((model) => ({ ...model, allowed: model.id === 'model-a' }));
  }
  if (options.noneAllowed) {
    initial.policy = { disabledModels: ['model-a', 'model-b', 'paid-only'], disabledProviders: ['glm', 'anthropic', 'openai'] };
    initial.models = initial.models.map((model) => ({ ...model, allowed: false }));
  }
  let current = structuredClone(initial);
  // Server-side restore point: captured at the first successful lock, cleared by restore/clear.
  let restorePoint: ModelPolicyRestorePoint | null = null;
  let revision = 0;
  let capabilityMode: 'auto_only' | 'manual' = 'auto_only';
  const accounts = () => ({ active_account: initial.email, items: [{ email: initial.email, workspaces: [{
    id: initial.workspace_id, name: '商业试用工作区', subscription_tier: 'business', plan_type: 'trial', eligible: true,
    model_capabilities: { mode: capabilityMode, models: capabilityMode === 'manual' ? [{ id: 'model-a', name: 'Model A', enabled: true }] : [] },
  }] }] });
  const responses: Record<string, unknown> = {
    '/admin/verify': { authenticated: true, password_configured: true, admin_enabled: true },
    '/admin/config': { config: { default_model: 'auto', features: {} }, models: [{ id: 'auto', name: 'Auto' }, { id: 'model-a', name: 'Model A' }] },
    '/admin/version': { version: 'test', default_model: 'auto', model_count: 1 },
    '/healthz': { ok: true, session_ready: true },
    '/admin/accounts/ai-usage': { accounts: [] },
    '/admin/conversations': { items: [] },
  };
  function applyWrite(input: ModelPolicyEdit) {
    const next = (value: Omit<ModelPolicySnapshot, 'revision'>) => ({ ...value, revision: `updated-${++revision}` });
    if (input.action === 'lock') {
      if (current.models.filter((model) => model.allowed).length === 1 && current.models.find((model) => model.id === input.model_id)?.allowed) return;
      if (!restorePoint) restorePoint = { policy: structuredClone(current.policy), present: current.policy_present, saved_at: '2026-09-20T08:30:00Z' };
      current = next({
        ...current, scope: input.scope,
        policy: { disabledModels: ['model-b', 'paid-only', 'retired-model'], disabledProviders: ['anthropic', 'openai'] },
        policy_present: true,
        models: current.models.map((model) => ({ ...model, allowed: model.id === input.model_id })),
      });
    } else if (input.action === 'restore') {
      const point = restorePoint!;
      current = next({
        ...current, policy: structuredClone(point.policy), policy_present: point.present,
        models: current.models.map((model) => ({ ...model, allowed: !point.policy.disabledModels.includes(model.id) && !point.policy.disabledProviders.includes(model.provider) })),
      });
      restorePoint = null;
    } else {
      current = next({
        ...current, policy: { disabledModels: [], disabledProviders: [] }, policy_present: false,
        models: current.models.map((model) => ({ ...model, allowed: true })),
      });
      restorePoint = null;
    }
  }
  await page.route('**/*', async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === '/admin/accounts/model-policy') {
      if (route.request().method() === 'GET') {
        expect(url.searchParams.get('email')).toBe(initial.email);
        expect(url.searchParams.get('workspace_id')).toBe(initial.workspace_id);
        reads.push(url.searchParams.get('scope')!);
        return route.fulfill({ json: { ...current, scope: reads.at(-1), restore_point: restorePoint } });
      }
      const input: ModelPolicyEdit = route.request().postDataJSON();
      writes.push(input);
      if (options.error && !options.errorAfterWrite) return route.fulfill({ status: options.error, json: { detail: '设置未确认，请重新读取' } });
      if (input.revision !== current.revision) return route.fulfill({ status: 409, json: { detail: '设置已被修改，请重新读取' } });
      if (input.action === 'restore' && !restorePoint) return route.fulfill({ status: 409, json: { detail: '没有可恢复的设置' } });
      applyWrite(input);
      if (options.error) return route.fulfill({ status: options.error, json: { detail: '设置未确认，请重新读取' } });
      return route.fulfill({ json: { ...current, restore_point: restorePoint } });
    }
    if (url.pathname === '/admin/accounts/refresh-models' && route.request().method() === 'POST') {
      capabilityRefreshes.push(route.request().postDataJSON());
      capabilityMode = 'manual';
      return route.fulfill({ json: { success: true } });
    }
    if (url.pathname === '/admin/accounts') { accountReads += 1; return route.fulfill({ json: accounts() }); }
    if (url.pathname in responses) return route.fulfill({ json: responses[url.pathname] });
    if (url.pathname === '/admin/events') return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    return route.continue();
  });
  return {
    initial, reads, writes, capabilityRefreshes,
    accountReads: () => accountReads,
    upstream: () => current,
    setUpstreamSnapshot: (value: ModelPolicySnapshot) => { current = structuredClone(value); },
  };
}

async function openPolicy(page: Page) {
  await page.goto('/admin');
  if (page.viewportSize()!.width < 1024) await page.getByRole('button', { name: '打开导航', exact: true }).click();
  await page.getByRole('button', { name: '账号与工作区', exact: true }).click();
  const panel = page.getByRole('region', { name: '工作区模型设置', exact: true });
  await panel.scrollIntoViewIfNeeded();
  await expect(panel.getByRole('button', { name: '读取模型设置' })).toBeEnabled();
  return panel;
}

test('workspace policy writes require an explicit apply and restore uses the server restore point', async ({ page }, testInfo) => {
  const { initial, reads, writes, upstream } = await mockModelPolicy(page);
  const panel = await openPolicy(page);
  expect(reads).toHaveLength(0);
  expect(writes).toHaveLength(0);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await expect(panel).toContainText('工作区所有者');
  await expect(panel).toContainText('暂无恢复点');
  await expect(panel.getByRole('button', { name: '恢复修改前设置' })).toBeDisabled();
  const model = panel.getByRole('combobox', { name: '工作区目标模型' });
  await model.click();
  await expect(page.getByRole('option', { name: /Paid Only/ })).toBeDisabled();
  await page.getByRole('option', { name: 'Model A', exact: true }).click();
  expect(writes).toHaveLength(0);
  await panel.getByRole('button', { name: '应用到整个工作区', exact: true }).click();
  await expect(panel.getByRole('status')).toContainText('保存成功');
  expect(writes).toEqual([{ email: initial.email, workspace_id: initial.workspace_id, scope: 'personal', revision: 'initial', action: 'lock', model_id: 'model-a' }]);
  await expect(panel).toContainText('服务器已保存修改前设置');
  await panel.getByRole('button', { name: '应用到整个工作区', exact: true }).click();
  await expect.poll(() => writes.length).toBe(2);
  await expect(panel.getByRole('button', { name: '恢复修改前设置' })).toBeEnabled();
  await panel.screenshot({ path: testInfo.outputPath('model-policy.png') });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await panel.getByRole('button', { name: '恢复修改前设置' }).click();
  await expect(panel.getByRole('status')).toContainText('已恢复');
  expect(writes[2]).toEqual({ email: initial.email, workspace_id: initial.workspace_id, scope: 'personal', revision: 'updated-1', action: 'restore' });
  expect(upstream().policy).toEqual(initial.policy);
  await expect(panel.getByRole('button', { name: '恢复修改前设置' })).toBeDisabled();
  await panel.getByRole('combobox', { name: '代理类型' }).click();
  await page.getByRole('option', { name: '自定义代理', exact: true }).click();
  expect(reads).toEqual(['personal']);
  expect(writes).toHaveLength(3);
  await expect(model).toHaveCount(0);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await expect(model).toBeEnabled();
  expect(reads).toEqual(['personal', 'custom']);
  await model.click();
  await page.getByRole('option', { name: 'Model A', exact: true }).click();
  await panel.getByRole('button', { name: '应用到整个工作区', exact: true }).click();
  await expect(panel.getByRole('status')).toContainText('保存成功');
  expect(writes[3].scope).toBe('custom');
});

test('the restore point survives a page reload because it lives on the server', async ({ page }) => {
  const { writes } = await mockModelPolicy(page);
  let panel = await openPolicy(page);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await panel.getByRole('combobox', { name: '工作区目标模型' }).click();
  await page.getByRole('option', { name: 'Model B', exact: true }).click();
  await panel.getByRole('button', { name: '应用到整个工作区', exact: true }).click();
  await expect(panel.getByRole('status')).toContainText('模型能力已刷新');
  panel = await openPolicy(page);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await expect(panel).toContainText('服务器已保存修改前设置');
  await expect(panel).toContainText('排除 1 个模型、0 个供应商');
  await panel.getByRole('button', { name: '恢复修改前设置' }).click();
  await expect(panel.getByRole('status')).toContainText('已恢复');
  expect(writes.map((write) => write.action)).toEqual(['lock', 'restore']);
});

for (const role of ['member', 'unknown']) {
  test(`${role} can read workspace policy but cannot edit it`, async ({ page }) => {
    const { writes } = await mockModelPolicy(page, { role });
    const panel = await openPolicy(page);
    await panel.getByRole('button', { name: '读取模型设置' }).click();
    await expect(panel).toContainText('当前仅可查看');
    await expect(panel.getByRole('combobox', { name: '工作区目标模型' })).toBeDisabled();
    await expect(panel.getByRole('button', { name: '应用到整个工作区' })).toBeDisabled();
    await expect(panel.getByRole('button', { name: '清除限制' })).toBeDisabled();
    await expect(panel.getByRole('button', { name: '恢复修改前设置' })).toBeDisabled();
    expect(writes).toHaveLength(0);
  });
}

for (const error of [403, 409, 429, 502]) {
  test(`policy save ${error} requires a manual reread without retries`, async ({ page }) => {
    // A 502 may arrive after the upstream write landed; the reread then shows
    // the server-side restore point. Rejected writes leave nothing to restore.
    const { writes, reads, capabilityRefreshes } = await mockModelPolicy(page, { error, errorAfterWrite: error === 502 });
    const panel = await openPolicy(page);
    await panel.getByRole('button', { name: '读取模型设置' }).click();
    await panel.getByRole('combobox', { name: '工作区目标模型' }).click();
    await page.getByRole('option', { name: 'Model A', exact: true }).click();
    await panel.getByRole('button', { name: '应用到整个工作区' }).click();
    await expect(panel.getByRole('status')).toContainText('请先重新读取');
    await expect(panel.getByRole('button', { name: '应用到整个工作区' })).toHaveCount(0);
    expect(writes).toHaveLength(1);
    expect(reads).toHaveLength(1);
    expect(capabilityRefreshes).toHaveLength(0);
    await panel.getByRole('button', { name: '读取模型设置' }).click();
    const restore = panel.getByRole('button', { name: '恢复修改前设置' });
    if (error === 502) await expect(restore).toBeEnabled();
    else await expect(restore).toBeDisabled();
    expect(writes).toHaveLength(1);
    expect(reads).toHaveLength(2);
  });
}

test('a rejected edit does not become the restore point for a later successful edit', async ({ page }) => {
  const options: MockOptions = { error: 409 };
  const { initial, writes, setUpstreamSnapshot, upstream } = await mockModelPolicy(page, options);
  const panel = await openPolicy(page);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await panel.getByRole('combobox', { name: '工作区目标模型' }).click();
  await page.getByRole('option', { name: 'Model A', exact: true }).click();
  await panel.getByRole('button', { name: '应用到整个工作区' }).click();
  await expect(panel.getByRole('status')).toContainText('请先重新读取');
  const external = { ...initial, revision: 'external-edit', policy: { disabledModels: ['external-model'], disabledProviders: [] } };
  setUpstreamSnapshot(external);
  options.error = undefined;
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await panel.getByRole('combobox', { name: '工作区目标模型' }).click();
  await page.getByRole('option', { name: 'Model A', exact: true }).click();
  await panel.getByRole('button', { name: '应用到整个工作区' }).click();
  await expect(panel.getByRole('status')).toContainText('保存成功');
  await panel.getByRole('button', { name: '恢复修改前设置' }).click();
  await expect(panel.getByRole('status')).toContainText('已恢复');
  expect(writes[2]).toMatchObject({ action: 'restore', revision: 'updated-1' });
  expect(upstream().policy).toEqual(external.policy);
});

test('applying an unchanged policy does not enable restore', async ({ page }) => {
  await mockModelPolicy(page, { locked: true });
  const panel = await openPolicy(page);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await panel.getByRole('button', { name: '应用到整个工作区' }).click();
  await expect(panel.getByRole('status')).toContainText('保存成功');
  await expect(panel.getByRole('button', { name: '恢复修改前设置' })).toBeDisabled();
});

test('clearing the policy asks for confirmation and removes the restriction', async ({ page }) => {
  const { initial, writes, capabilityRefreshes, upstream } = await mockModelPolicy(page);
  const panel = await openPolicy(page);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  const clear = panel.getByRole('button', { name: '清除限制' });
  await clear.click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('整个工作区');
  await dialog.getByRole('button', { name: '取消' }).click();
  await expect(dialog).toHaveCount(0);
  expect(writes).toHaveLength(0);
  await clear.click();
  await page.getByRole('dialog').getByRole('button', { name: '确认清除' }).click();
  await expect(panel.getByRole('status')).toContainText('已清除模型限制');
  expect(writes).toEqual([{ email: initial.email, workspace_id: initial.workspace_id, scope: 'personal', revision: 'initial', action: 'clear' }]);
  expect(upstream().policy_present).toBe(false);
  await expect(panel).toContainText('尚未设置额外模型限制');
  await expect(clear).toBeDisabled();
  await expect.poll(() => capabilityRefreshes).toEqual([{ email: initial.email, workspace_id: initial.workspace_id }]);
});

test('a policy that allows no catalog model shows a prominent warning', async ({ page }) => {
  await mockModelPolicy(page, { noneAllowed: true });
  const panel = await openPolicy(page);
  await expect(panel.getByRole('alert')).toHaveCount(0);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await expect(panel.getByRole('alert')).toContainText('没有任何允许的模型');
  await panel.getByRole('combobox', { name: '工作区目标模型' }).click();
  await page.getByRole('option', { name: 'Model A', exact: true }).click();
  await panel.getByRole('button', { name: '应用到整个工作区' }).click();
  await expect(panel.getByRole('status')).toContainText('保存成功');
  await expect(panel.getByRole('alert')).toHaveCount(0);
});

test('a successful policy change refreshes model capabilities and the account list', async ({ page }) => {
  const { initial, capabilityRefreshes, accountReads } = await mockModelPolicy(page);
  const panel = await openPolicy(page);
  await expect(page.getByText('模型选择：仅 Auto')).toBeVisible();
  const readsBefore = accountReads();
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await panel.getByRole('combobox', { name: '工作区目标模型' }).click();
  await page.getByRole('option', { name: 'Model A', exact: true }).click();
  await panel.getByRole('button', { name: '应用到整个工作区' }).click();
  await expect(panel.getByRole('status')).toContainText('模型能力已刷新');
  expect(capabilityRefreshes).toEqual([{ email: initial.email, workspace_id: initial.workspace_id }]);
  expect(accountReads()).toBeGreaterThan(readsBefore);
  await expect(page.getByText('模型选择：支持手动选择')).toBeVisible();
});
