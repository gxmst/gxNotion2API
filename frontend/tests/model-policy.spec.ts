import { expect, test, type Page } from '@playwright/test';
import type { ModelPolicyEdit, ModelPolicySnapshot } from '../lib/services/admin/types';

async function mockModelPolicy(page: Page, options: { role?: string; error?: number; locked?: boolean } = {}) {
  page.on('pageerror', (error) => { throw error; });
  const writes: ModelPolicyEdit[] = [];
  const reads: string[] = [];
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
  let current = structuredClone(initial);
  const responses: Record<string, unknown> = {
    '/admin/verify': { authenticated: true, password_configured: true, admin_enabled: true },
    '/admin/config': { config: { default_model: 'auto', features: {} }, models: [{ id: 'auto', name: 'Auto' }] },
    '/admin/version': { version: 'test', default_model: 'auto', model_count: 1 },
    '/healthz': { ok: true, session_ready: true },
    '/admin/accounts': { active_account: initial.email, items: [{ email: initial.email, workspaces: [{
      id: initial.workspace_id, name: '商业试用工作区', subscription_tier: 'business', plan_type: 'trial', eligible: true,
      model_capabilities: { mode: 'auto_only', models: [] },
    }] }] },
    '/admin/accounts/ai-usage': { accounts: [] },
    '/admin/conversations': { items: [] },
  };
  await page.route('**/*', async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === '/admin/accounts/model-policy') {
      if (route.request().method() === 'GET') {
        expect(url.searchParams.get('email')).toBe(initial.email);
        expect(url.searchParams.get('workspace_id')).toBe(initial.workspace_id);
        reads.push(url.searchParams.get('scope')!);
        return route.fulfill({ json: { ...current, scope: reads.at(-1) } });
      }
      const input: ModelPolicyEdit = route.request().postDataJSON();
      writes.push(input);
      if (options.error) return route.fulfill({ status: options.error, json: { detail: '设置未确认，请重新读取' } });
      if (input.action === 'lock' && current.models.filter((model) => model.allowed).length === 1 && current.models.find((model) => model.id === input.model_id)?.allowed) {
        return route.fulfill({ json: current });
      }
      current = input.action === 'restore' ? structuredClone(initial) : {
        ...current, scope: input.scope, revision: `updated-${writes.length}`,
        policy: { disabledModels: ['model-b', 'paid-only', 'retired-model'], disabledProviders: ['anthropic', 'openai'] },
        models: current.models.map((model) => ({ ...model, allowed: model.id === input.model_id })),
      };
      return route.fulfill({ json: current });
    }
    if (url.pathname in responses) return route.fulfill({ json: responses[url.pathname] });
    if (url.pathname === '/admin/events') return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    return route.continue();
  });
  return { initial, reads, writes, setUpstreamSnapshot: (value: ModelPolicySnapshot) => { current = structuredClone(value); } };
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

test('workspace policy writes require an explicit apply and restore keeps the original snapshot', async ({ page }, testInfo) => {
  const { initial, reads, writes } = await mockModelPolicy(page);
  const panel = await openPolicy(page);
  expect(reads).toHaveLength(0);
  expect(writes).toHaveLength(0);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await expect(panel).toContainText('工作区所有者');
  const model = panel.getByRole('combobox', { name: '工作区目标模型' });
  await model.click();
  await expect(page.getByRole('option', { name: /Paid Only/ })).toBeDisabled();
  await page.getByRole('option', { name: 'Model A', exact: true }).click();
  expect(writes).toHaveLength(0);
  await panel.getByRole('button', { name: '应用到整个工作区', exact: true }).click();
  await expect(panel.getByRole('status')).toContainText('保存成功');
  expect(writes).toEqual([{ email: initial.email, workspace_id: initial.workspace_id, scope: 'personal', revision: 'initial', action: 'lock', model_id: 'model-a' }]);
  await panel.getByRole('button', { name: '应用到整个工作区', exact: true }).click();
  await expect.poll(() => writes.length).toBe(2);
  await expect(panel.getByRole('button', { name: '恢复本次修改前设置' })).toBeEnabled();
  await panel.screenshot({ path: testInfo.outputPath('model-policy.png') });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await panel.getByRole('button', { name: '恢复本次修改前设置' }).click();
  await expect(panel.getByRole('status')).toContainText('已恢复');
  expect(writes[2]).toMatchObject({ action: 'restore', restore_policy: initial.policy, restore_present: true, revision: 'updated-1' });
  await expect(panel.getByRole('button', { name: '恢复本次修改前设置' })).toBeDisabled();
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

for (const role of ['member', 'unknown']) {
  test(`${role} can read workspace policy but cannot edit it`, async ({ page }) => {
    const { writes } = await mockModelPolicy(page, { role });
    const panel = await openPolicy(page);
    await panel.getByRole('button', { name: '读取模型设置' }).click();
    await expect(panel).toContainText('当前仅可查看');
    await expect(panel.getByRole('combobox', { name: '工作区目标模型' })).toBeDisabled();
    await expect(panel.getByRole('button', { name: '应用到整个工作区' })).toBeDisabled();
    expect(writes).toHaveLength(0);
  });
}

for (const error of [403, 409, 429, 502]) {
  test(`policy save ${error} requires a manual reread without retries`, async ({ page }) => {
    const { writes, reads } = await mockModelPolicy(page, { error });
    const panel = await openPolicy(page);
    await panel.getByRole('button', { name: '读取模型设置' }).click();
    await panel.getByRole('combobox', { name: '工作区目标模型' }).click();
    await page.getByRole('option', { name: 'Model A', exact: true }).click();
    await panel.getByRole('button', { name: '应用到整个工作区' }).click();
    await expect(panel.getByRole('status')).toContainText('请先重新读取');
    await expect(panel.getByRole('button', { name: '应用到整个工作区' })).toHaveCount(0);
    expect(writes).toHaveLength(1);
    expect(reads).toHaveLength(1);
    await panel.getByRole('button', { name: '读取模型设置' }).click();
    const restore = panel.getByRole('button', { name: '恢复本次修改前设置' });
    if (error === 502) await expect(restore).toBeEnabled();
    else await expect(restore).toBeDisabled();
    expect(writes).toHaveLength(1);
    expect(reads).toHaveLength(2);
  });
}

test('a rejected edit does not become the restore point for a later successful edit', async ({ page }) => {
  const options: { error?: number } = { error: 409 };
  const { initial, writes, setUpstreamSnapshot } = await mockModelPolicy(page, options);
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
  await panel.getByRole('button', { name: '恢复本次修改前设置' }).click();
  await expect(panel.getByRole('status')).toContainText('已恢复');
  expect(writes[2].restore_policy).toEqual(external.policy);
});

test('applying an unchanged policy does not enable restore', async ({ page }) => {
  await mockModelPolicy(page, { locked: true });
  const panel = await openPolicy(page);
  await panel.getByRole('button', { name: '读取模型设置' }).click();
  await panel.getByRole('button', { name: '应用到整个工作区' }).click();
  await expect(panel.getByRole('status')).toContainText('保存成功');
  await expect(panel.getByRole('button', { name: '恢复本次修改前设置' })).toBeDisabled();
});
