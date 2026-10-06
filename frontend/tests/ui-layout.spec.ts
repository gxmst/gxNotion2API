import { test, expect, type Page } from '@playwright/test';

async function fixture(page: Page) {
  const deleted: string[] = [];
  const account = { email: 'owner@example.com', user_name: '工作室', status: 'ready', workspaces: [
    { id: 'workspace-one', name: '产品设计工作室', plan_type: 'business', eligible: true, default: true, model_capabilities: { mode: 'auto_only', models: [] } },
    { id: 'workspace-two', name: '个人研究空间', plan_type: 'business', eligible: true, model_capabilities: { mode: 'manual', models: [{ id: 'model-a', name: 'Model A', enabled: true }] } },
  ] };
  const conversation = { id: 'conversation-one', title: '整理产品发布计划', model: 'auto', origin: 'local', status: 'completed', account_email: account.email, preview: '将设计、开发和发布拆成三个阶段。', messages: [{ id: 'm1', role: 'user', content: '帮我整理产品发布计划' }, { id: 'm2', role: 'assistant', content: '将设计、开发和发布拆成三个阶段。' }] };
  page.on('pageerror', (error) => { throw error; });
  await page.route('**/*', async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const models = [{ id: 'auto', name: 'Auto', enabled: true }, { id: 'model-a', name: 'Model A', enabled: true }];
    const responses: Record<string, unknown> = {
      '/admin/verify': { authenticated: true, password_configured: true, admin_enabled: true },
      '/admin/config': { config: { host: '127.0.0.1', port: 8787, default_model: 'auto', features: {} }, models, session: { user_email: account.email, space_name: '产品设计工作室' } },
      '/admin/version': { version: 'preview', default_model: 'auto' },
      '/healthz': { ok: true, session_ready: true },
      '/admin/accounts': { items: [account], active_account: account.email, session_ready: true },
      '/admin/accounts/ai-usage': { accounts: [] },
      '/admin/accounts/ai-usage/workspace': {},
      '/admin/conversations': { items: deleted.length ? [] : [conversation] },
      '/admin/conversations/conversation-one': { item: conversation },
    };
    if (route.request().method() === 'DELETE' || path === '/admin/conversations/batch-delete') {
      deleted.push(path); return route.fulfill({ json: { success: true } });
    }
    if (path === '/admin/events') return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    if (path in responses) return route.fulfill({ json: responses[path] });
    return route.continue();
  });
  return deleted;
}

test('all pages fit the viewport in both themes', async ({ page }, testInfo) => {
  test.setTimeout(60_000);
  await fixture(page);
  for (const theme of ['light', 'dark']) {
    for (const tab of ['tester', 'accounts', 'conversations', 'dashboard', 'models', 'settings']) {
      await page.goto('/admin?tab=' + tab);
      await expect(page.getByRole('combobox', { name: '主题模式' })).toBeVisible();
      await page.getByRole('combobox', { name: '主题模式' }).click();
      await page.getByRole('option', { name: theme === 'light' ? '浅色' : '深色', exact: true }).click();
      await expect(page.locator('html')).toHaveClass(new RegExp(theme));
      await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      if (tab === 'tester') {
        await expect(page.getByRole('textbox', { name: '消息', exact: true })).toBeVisible();
        const box = await page.getByRole('textbox', { name: '消息', exact: true }).boundingBox();
        expect(box!.y + box!.height).toBeLessThanOrEqual(page.viewportSize()!.height);
      }
      await page.screenshot({ path: testInfo.outputPath(tab + '-' + theme + '.png'), fullPage: true });
      const categories = tab === 'accounts' ? ['模型与套餐', '账号信息'] : tab === 'settings' ? ['上游连接（高级）', '聊天与协议', '提示词策略', '安全与存储', '调试与模型映射', '配置文件'] : [];
      for (const category of categories) {
        await page.getByRole('button', { name: category, exact: true }).click();
        await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      }

    }
  }
});

test('batch delete can be cancelled before any request is sent', async ({ page }) => {
  const deleted = await fixture(page);
  await page.goto('/admin?tab=conversations');
  await page.getByRole('button', { name: '全选', exact: true }).click();
  page.once('dialog', (dialog) => void dialog.dismiss());
  await page.getByRole('button', { name: /批量删除/ }).click();
  expect(deleted).toHaveLength(0);
  page.once('dialog', (dialog) => void dialog.accept());
  await page.getByRole('button', { name: /批量删除/ }).click();
  await expect.poll(() => deleted.length).toBe(1);
});

test('login is usable on a short viewport and failed authentication stays visible', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: page.viewportSize()!.width, height: 540 });
  await page.route('**/admin/verify', (route) => route.fulfill({ json: { authenticated: false, password_required: true, password_configured: true, admin_enabled: true } }));
  await page.route('**/admin/login', (route) => route.fulfill({ status: 401, json: { error: { message: '密码不正确' } } }));
  await page.goto('/admin');
  await page.getByLabel('管理密码', { exact: true }).fill('incorrect');
  await page.getByRole('button', { name: '登录控制台' }).click();
  await expect(page.getByRole('region', { name: '登录' })).toContainText('密码不正确');
  await page.screenshot({ path: testInfo.outputPath('login.png'), fullPage: true });
});

test('notes are read explicitly and references stay in the draft', async ({ page }) => {
  await fixture(page);
  const reads: string[] = [];
  await page.route('**/admin/notes?**', async (route) => {
    reads.push(route.request().url());
    const detail = new URL(route.request().url()).searchParams.has('page_id');
    await route.fulfill({ json: detail ? { item: { id: 'note-one', title: '测试笔记', text: '可以引用的正文', partial: true } } : { items: [{ id: 'note-one', title: '测试笔记' }] } });
  });
  await page.goto('/admin?tab=tester');
  await page.getByRole('button', { name: '工作区笔记', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '工作区笔记', exact: true });
  await expect(dialog.getByRole('button', { name: '读取笔记', exact: true })).toBeVisible();
  expect(reads).toHaveLength(0);
  await dialog.getByRole('button', { name: '读取笔记', exact: true }).click();
  await dialog.getByRole('button', { name: '测试笔记', exact: true }).click();
  await expect(dialog).toContainText('这是部分文本预览');
  await dialog.getByRole('button', { name: '引用到输入框', exact: true }).click();
  await expect(page.getByRole('textbox', { name: '消息', exact: true })).toHaveValue(/可以引用的正文/);
  expect(reads).toHaveLength(2);
});

test('quota shows rolling and monthly reset information', async ({ page }) => {
  await fixture(page);
  const report = { status: 'ok', email: 'owner@example.com', space_id: 'workspace-one', fetched_at: new Date().toISOString(), usage: { rate_limit: {
    short: { used: 10, limit: 100, label: '6h', resets_at_ms: Date.now() + 21540000 },
    long: { used: 25, limit: 100, period_end_ms: new Date('2026-10-23T12:00:00+08:00').getTime() },
  } } };
  await page.route('**/admin/accounts/ai-usage', (route) => route.fulfill({ json: { accounts: [report] } }));
  await page.route('**/admin/accounts/ai-usage?**', (route) => route.fulfill({ json: { accounts: [report] } }));
  await page.route('**/admin/accounts/ai-usage/workspace?**', (route) => route.fulfill({ json: report }));
  await page.goto('/admin?tab=tester');
  await page.locator('.chat-quota-chip').click();
  const dialog = page.getByRole('dialog', { name: '工作区额度', exact: true });
  await expect(dialog).toContainText('已使用 10%');
  await expect(dialog).toContainText('已使用 25%');
  await expect(dialog).toContainText('小时');
  await expect(dialog).toContainText('10月23日');
});


test('failed note switch clears the previous reference', async ({ page }) => {
  await fixture(page);
  await page.route('**/admin/notes?**', async (route) => {
    const id = new URL(route.request().url()).searchParams.get('page_id');
    if (id === 'bad') return route.fulfill({ status: 403, json: { detail: '无法读取' } });
    return route.fulfill({ json: id ? { item: { id: 'good', title: '可读笔记', text: '旧正文', partial: false } } : { items: [{ id: 'good', title: '可读笔记' }, { id: 'bad', title: '失效笔记' }] } });
  });
  await page.goto('/admin?tab=tester');
  await page.getByRole('button', { name: '工作区笔记', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '工作区笔记', exact: true });
  await dialog.getByRole('button', { name: '读取笔记', exact: true }).click();
  await dialog.getByRole('button', { name: '可读笔记', exact: true }).click();
  await expect(dialog.getByRole('button', { name: '引用到输入框' })).toBeVisible();
  await dialog.getByRole('button', { name: '失效笔记', exact: true }).click();
  await expect(dialog.getByRole('alert')).toBeVisible();
  await expect(dialog.getByRole('button', { name: '引用到输入框' })).toHaveCount(0);
  await expect(dialog).not.toContainText('旧正文');
});


test('notes remain available for a conversation without workspace metadata', async ({ page }) => {
  await fixture(page);
  const sources: string[] = [];
  await page.route('**/admin/notes?**', async (route) => {
    sources.push(new URL(route.request().url()).searchParams.get('source') || 'shared');
    await route.fulfill({ json: { items: [{ id: 'private-page', title: '私人笔记' }] } });
  });
  await page.goto('/admin?tab=tester');
  if (page.viewportSize()!.width < 1024) await page.getByRole('button', { name: '打开导航', exact: true }).click();
  await page.locator('.chat-conversation-open:visible').first().click();
  await page.getByRole('button', { name: '工作区笔记', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '工作区笔记', exact: true });
  await expect(dialog.getByRole('combobox', { name: '笔记工作区' })).toBeEnabled();
  await dialog.getByRole('combobox', { name: '笔记范围' }).click();
  await page.getByRole('option', { name: '私人页面', exact: true }).click();
  expect(sources).toHaveLength(0);
  await dialog.getByRole('button', { name: '读取笔记', exact: true }).click();
  await expect(dialog.getByRole('button', { name: '私人笔记', exact: true })).toBeVisible();
  expect(sources).toEqual(['private']);
});

test('empty composer uses one line and grows with text', async ({ page }) => {
  await fixture(page);
  await page.goto('/admin?tab=tester');
  const input = page.getByRole('textbox', { name: '消息', exact: true });
  await expect(input).toBeVisible();
  await expect.poll(async () => (await input.boundingBox())!.height).toBeLessThan(40);
  await input.fill('第一行\n第二行\n第三行');
  await expect.poll(async () => (await input.boundingBox())!.height).toBeGreaterThan(65);
  await input.fill('');
  await expect.poll(async () => (await input.boundingBox())!.height).toBeLessThan(40);
});


for (const scenario of [
  { name: 'verified Opus name without a refreshed catalog', raw: 'albuquerque-quinn', models: [], expected: 'Opus 5.5' },
  { name: 'future model from its catalog alias', raw: 'future-runtime', models: [{ id: 'gpt-8', name: 'GPT 8', aliases: ['future-runtime'] }], expected: 'GPT 8' },
  { name: 'live mapping takes precedence over verified names', raw: 'albuquerque-quinn', models: [{ id: 'current-opus', name: 'Opus Current', notion_model: 'albuquerque-quinn' }], expected: 'Opus Current' },
  { name: 'contradicting provider does not get a guessed name', raw: 'future-runtime', models: [{ id: 'gpt-8', name: 'GPT 8', family: 'openai', aliases: ['future-runtime'] }], expected: '未识别模型（future-runtime）' },
  { name: 'ambiguous aliases stay unidentified', raw: 'shared-runtime', models: [{ id: 'a', name: 'Model A', aliases: ['shared-runtime'] }, { id: 'b', name: 'Model B', aliases: ['shared-runtime'] }], expected: '未识别模型（shared-runtime）' },
]) {
  test('model evidence uses ' + scenario.name, async ({ page }, testInfo) => {
    await fixture(page);
    await page.route('**/admin/config', (route) => route.fulfill({ json: { config: { default_model: 'auto', features: {} }, models: [{ id: 'auto', name: 'Auto' }, ...scenario.models] } }));
    await page.route('**/admin/conversations/conversation-one*', (route) => route.fulfill({ json: { item: { id: 'conversation-one', title: '整理产品发布计划', messages: [
      { id: 'user', role: 'user', content: '帮我整理产品发布计划。' },
      { id: 'answer', role: 'assistant', content: '### 先确定发布范围\n\n把这次发布拆成三个阶段：\n\n1. **准备**：确认核心功能与验收标准。\n2. **验证**：检查关键流程并收集反馈。\n3. **发布**：保留回滚版本，观察运行状态。', model_selection_mode: 'auto', model_observations: [{ model: scenario.raw, provider: 'anthropic', source: 'stream' }] },
    ] } } }));
    await page.goto('/admin?tab=tester');
    if (page.viewportSize()!.width < 1024) await page.getByRole('button', { name: '打开导航', exact: true }).click();
    await page.locator('.chat-conversation-open:visible').first().click();
    const evidence = page.getByTestId('model-evidence');
    await expect(evidence.locator('summary')).toContainText('实际模型：' + scenario.expected);
    await expect(evidence.locator('.model-evidence-details')).not.toBeVisible();
    if (scenario.expected === 'Opus 5.5') {
      await expect(evidence.locator('summary')).not.toContainText(scenario.raw);
      for (const theme of ['浅色', '深色']) {
        await page.getByRole('combobox', { name: '主题模式' }).click();
        await page.getByRole('option', { name: theme, exact: true }).click();
        await page.screenshot({ path: testInfo.outputPath(theme === '深色' ? 'chat-dark.png' : 'chat-light.png'), fullPage: true });
      }
    }
    await evidence.locator('summary').click();
    await expect(evidence.locator('.model-evidence-details')).toContainText(scenario.raw);
  });
}
