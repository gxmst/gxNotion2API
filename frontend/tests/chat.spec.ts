import { expect, test, type Page } from '@playwright/test';
import type { ChatRunInput, ConversationDetail, ConversationMessage } from '../lib/services/admin/types';

const businessAccount = 'test@example.com';

async function mockAdmin(page: Page, options: { truncated?: boolean; lengthStop?: boolean; deferred?: boolean; gradual?: boolean; restricted?: boolean; remoteOnly?: boolean } = {}) {
  const requests: Array<ChatRunInput & { stream: boolean }> = [];
  const conversations = new Map<string, ConversationDetail>();
  if (options.remoteOnly) {
    // A transcript that only exists in Notion: the bridge holds no local row,
    // so rename, delete, and per-message edit have nothing to write to.
    conversations.set('conv_notion_thread_remote', {
      id: 'conv_notion_thread_remote',
      title: 'Notion 上的历史对话',
      messages: [
        { id: 'remote-user', role: 'user', content: 'Notion 里的问题', status: 'completed' },
        { id: 'remote-assistant', role: 'assistant', content: 'Notion 里的回答', status: 'completed' },
      ],
      model: 'test-model',
      status: 'completed',
      account_email: businessAccount,
      space_id: 'business-trial',
      remote_only: true,
    });
  }
  let release = () => {};
  const pending = new Promise<void>((resolve) => { release = resolve; });
  const responses: Record<string, unknown> = {
    '/admin/verify': { authenticated: true, password_configured: true, admin_enabled: true },
    '/admin/config': { config: { default_model: 'test-model', features: { use_web_search: false } }, models: [{ id: 'test-model', name: 'Test model' }] },
    '/admin/version': { version: 'test', default_model: 'test-model', model_count: 1 },
    '/healthz': { ok: true, session_ready: true },
    '/admin/accounts': { items: [
      { email: businessAccount, workspaces: [
        { id: 'business-trial', name: '商业试用工作区', subscription_tier: 'business', plan_type: 'trial', eligible: true, model_capabilities: options.restricted ? { mode: 'auto_only', models: [], catalog: [{ id: 'test-model', name: 'Test model', notion_model: 'test-codename' }] } : { mode: 'manual', models: [{ id: 'test-model', name: 'Test model', notion_model: 'test-codename', enabled: true }] } },
        { id: 'free', name: '免费工作区', subscription_tier: 'free', eligible: false },
        { id: 'plus', name: 'Plus 工作区', subscription_tier: 'plus', eligible: false },
      ] },
      { email: 'team@example.com', workspaces: [{ id: 'enterprise', name: '企业工作区', subscription_tier: 'enterprise', eligible: true, model_capabilities: { mode: 'manual', models: [{ id: 'test-model', name: 'Test model', notion_model: 'test-codename', enabled: true }] } }] },
    ] },
  };
  await page.route('**/*', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path in responses) return route.fulfill({ json: responses[path] });
    if (path === '/admin/events') {
      return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    }
    if (path === '/admin/conversations') return route.fulfill({ json: { items: [...conversations.values()] } });
    if (path.startsWith('/admin/conversations/')) {
      const item = conversations.get(decodeURIComponent(path.split('/').pop()!));
      return route.fulfill({ status: item ? 200 : 404, json: item ? { item } : { detail: 'conversation not found' } });
    }
    if (path === '/admin/test') {
      const input = route.request().postDataJSON();
      requests.push(input);
      const answer = `**Reply ${requests.length}**\n\n${input.prompt}`;
      const messages: ConversationMessage[] = [
        { id: `user-${requests.length}`, role: 'user', content: input.prompt, status: 'completed' },
        { id: `answer-${requests.length}`, role: 'assistant', content: answer, status: options.truncated ? 'failed' : 'completed', requested_model: input.model,
          truncated: options.lengthStop,
          model_selection_mode: input.model === 'auto' ? 'auto' : 'manual', model_observations: [{ model: 'test-codename', source: 'thread_record' }] },
      ];
      const item: ConversationDetail = conversations.get(input.conversation_id) || {
        id: input.conversation_id, messages: [], model: input.model,
        title: input.prompt.slice(0, 60), account_email: input.account_email || businessAccount,
        space_id: input.workspace_id || 'business-trial',
      };
      item.messages = [...(item.messages || []), ...messages];
      item.status = options.truncated ? 'failed' : 'completed';
      conversations.set(input.conversation_id, item);
      if (options.deferred) await pending;
      if (options.gradual) {
        return route.continue({ url: new URL('/admin/__e2e/stream', route.request().url()).href });
      }
      const parts = ['**Reply ', `${requests.length}**\n\n`, input.prompt];
      const body = parts.map((content) => `data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\r\n\r\n`).join('');
      // A length stop is a completed connection carrying an incomplete answer,
      // which is a different case from a dropped stream.
      const finish = options.lengthStop
        ? `data: ${JSON.stringify({ choices: [{ delta: {}, finish_reason: 'length' }] })}\r\n\r\n`
        : '';
      return route.fulfill({
        contentType: 'text/event-stream',
        headers: { 'x-conversation-id': input.conversation_id },
        body: body + finish + (options.truncated ? '' : 'data: [DONE]\r\n\r\n'),
      });
    }
    return route.continue();
  });
  return { requests, release };
}

async function openChat(page: Page) {
  await page.goto('/admin');
  await expect(page.getByRole('textbox', { name: '消息', exact: true })).toBeEnabled();
}

async function send(page: Page, prompt: string) {
  await page.getByRole('textbox', { name: '消息', exact: true }).fill(prompt);
  await page.getByRole('button', { name: '发送', exact: true }).click();
}

async function openChatNavigation(page: Page) {
  if (page.viewportSize()!.width < 1024) await page.getByRole('button', { name: '打开导航', exact: true }).click();
}

test('two streamed turns reuse their conversation and restore history', async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const { requests } = await mockAdmin(page);
  await openChat(page);
  await page.screenshot({ path: testInfo.outputPath('welcome.png'), fullPage: true });
  const history = page.getByLabel('聊天记录');
  for (const [turn, prompt] of ['First question', 'Follow-up question'].entries()) {
    await send(page, prompt);
    await expect(history.locator('strong').last()).toHaveText(`Reply ${turn + 1}`);
    await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  }
  expect(requests).toHaveLength(2);
  expect(requests[0].stream).toBe(true);
  expect(requests[0].model).toBe('test-model');
  expect(requests[0].conversation_id).toBeTruthy();
  expect(requests[1].conversation_id).toBe(requests[0].conversation_id);
  await expect(history.locator('article')).toHaveCount(4);
  const composer = await page.locator('.chat-composer').boundingBox();
  const header = await page.locator('.chat-header').boundingBox();
  expect(composer!.y).toBeGreaterThan(header!.y + header!.height);
  expect(composer!.y + composer!.height).toBeLessThanOrEqual(page.viewportSize()!.height);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  expect(await page.evaluate(() => document.documentElement.scrollHeight <= window.innerHeight + 1)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath('chat.png'), fullPage: true });
  await page.getByRole('button', { name: '切换明暗主题' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.screenshot({ path: testInfo.outputPath('chat-dark.png'), fullPage: true });
  await page.getByRole('button', { name: '切换明暗主题' }).click();
  await openChat(page);
  await expect(history.locator('article')).toHaveCount(4);
  await expect(history.locator('strong').last()).toHaveText('Reply 2');
  expect(errors).toEqual([]);
});

test('a truncated stream preserves partial text and allows recovery', async ({ page }) => {
  await mockAdmin(page, { truncated: true });
  await openChat(page);
  await send(page, 'Interrupted question');
  const history = page.getByLabel('聊天记录');
  await expect(history.locator('strong')).toHaveText('Reply 1');
  await expect(page.locator('.chat-error')).toContainText('连接中断');
  await expect(history.getByText('未完成', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  await expect(page.getByRole('textbox', { name: '消息', exact: true })).toHaveValue('Interrupted question');
});

test('a length finish reason marks the answer as truncated', async ({ page }) => {
  await mockAdmin(page, { lengthStop: true });
  await openChat(page);
  await send(page, 'Long question');
  const history = page.getByLabel('聊天记录');
  await expect(history.locator('strong')).toHaveText('Reply 1');
  await expect(history.getByText('已截断', { exact: true })).toBeVisible();
  // The stream completed cleanly, so this is not the dropped-connection case.
  await expect(page.locator('.chat-error')).toHaveCount(0);
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
});

test('a remote-only transcript hides the controls that need a local row', async ({ page }) => {
  await mockAdmin(page, { remoteOnly: true });
  await openChat(page);
  const label = 'Notion 上的历史对话';
  const sidebar = page.getByLabel('历史会话');
  // The sidebar is an off-canvas drawer on narrow viewports, so it has to be
  // opened before any of its controls are in the accessibility tree.
  await openChatNavigation(page);

  // Rename and delete both write to the bridge's own store, which holds no row
  // for this transcript, so offering them would mean failing on save.
  await expect(sidebar.getByRole('button', { name: `重命名 ${label}` })).toHaveCount(0);
  await expect(sidebar.getByRole('button', { name: `删除 ${label}` })).toHaveCount(0);
  // Export only reads, so it stays available.
  await expect(sidebar.getByRole('button', { name: `导出 ${label}` })).toBeVisible();

  await sidebar.getByRole('button', { name: label, exact: true }).click();
  const history = page.getByLabel('聊天记录');
  await expect(history.getByText('Notion 里的回答')).toBeVisible();
  await expect(history.getByRole('button', { name: '编辑消息' }).first()).toBeDisabled();
});

test('a locally stored conversation keeps its rename and delete controls', async ({ page }) => {
  await mockAdmin(page);
  await openChat(page);
  await send(page, 'A question that creates a local row');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  // Check the transcript first: on mobile the sidebar is an overlay that takes
  // the main pane out of the accessibility tree while it is open.
  const history = page.getByLabel('聊天记录');
  await expect(history.getByRole('button', { name: '编辑消息' }).first()).toBeEnabled();
  const sidebar = page.getByLabel('历史会话');
  await openChatNavigation(page);
  // The gating must be specific to remote-only transcripts.
  await expect(sidebar.getByRole('button', { name: /^重命名 / }).first()).toBeEnabled();
  await expect(sidebar.getByRole('button', { name: /^删除 / }).first()).toBeEnabled();
});

test('new chats select an eligible workspace and existing chats keep their owner', async ({ page }) => {
  const { requests } = await mockAdmin(page);
  await openChat(page);
  const workspace = page.getByRole('combobox', { name: '商业工作区' });
  await workspace.click();
  await expect(page.getByRole('option', { name: /免费工作区|Plus 工作区/ })).toHaveCount(0);
  await page.getByRole('option', { name: /商业试用工作区/ }).click();
  await send(page, 'First workspace');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  expect(requests[0].account_email).toBe(businessAccount);
  expect(requests[0].workspace_id).toBe('business-trial');
  await expect(workspace).toBeDisabled();
  await openChat(page);
  await expect(workspace).toContainText('商业试用工作区');
  await send(page, 'Same workspace');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  expect(requests[1].workspace_id).toBe('business-trial');
  await page.getByRole('button', { name: '新对话', exact: true }).click();
  await workspace.click();
  await page.getByRole('option', { name: /企业工作区/ }).click();
  await send(page, 'Another workspace');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  expect(requests[2].account_email).toBe('team@example.com');
  expect(requests[2].workspace_id).toBe('enterprise');
  expect(requests[2].conversation_id).not.toBe(requests[0].conversation_id);
});

test('visiting management keeps the active generation and unsent draft', async ({ page }) => {
  const { requests, release } = await mockAdmin(page, { deferred: true });
  const failed: string[] = [];
  page.on('requestfailed', (request) => { if (request.url().endsWith('/admin/test')) failed.push(request.failure()?.errorText || 'failed'); });
  await openChat(page);
  await send(page, 'A long-running question');
  await expect.poll(() => requests.length).toBe(1);
  await page.getByRole('textbox', { name: '消息', exact: true }).fill('Draft for the next turn');
  await openChatNavigation(page);
  await page.getByRole('button', { name: /管理控制台/ }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  release();
  if (page.viewportSize()!.width < 1024) await page.getByRole('button', { name: '打开导航菜单', exact: true }).click();
  await page.getByRole('button', { name: '聊天', exact: true }).click();
  await expect(page.getByLabel('聊天记录').locator('strong')).toHaveText('Reply 1');
  await expect(page.getByRole('textbox', { name: '消息', exact: true })).toHaveValue('Draft for the next turn');
  expect(failed).toEqual([]);
});

test('reading earlier messages suspends automatic scrolling during a live stream', async ({ page }) => {
  await mockAdmin(page, { gradual: true });
  await openChat(page);
  await send(page, 'Stream a long answer');
  const history = page.getByLabel('聊天记录');
  await expect(history).toContainText('Paragraph 6');
  await history.evaluate((element) => { element.scrollTop = 0; });
  await expect(page.getByRole('button', { name: '回到最新' })).toBeVisible();
  await expect(history).toContainText('Paragraph 8');
  expect(await history.evaluate((element) => element.scrollTop)).toBeLessThan(20);
  await page.getByRole('button', { name: '回到最新' }).click();
  await expect.poll(() => history.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(90);
  await page.getByRole('button', { name: '停止', exact: true }).click();
  await expect(page.locator('.chat-error')).toContainText('已停止生成');
});

test('malformed saved state and HTTP-compatible ID generation do not break chat', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('notion2api-chat-session', JSON.stringify({ conversationID: 42, prompt: {}, model: [] }));
    Object.defineProperty(crypto, 'randomUUID', { value: undefined, configurable: true });
  });
  const { requests } = await mockAdmin(page);
  await openChat(page);
  await send(page, 'Hello without randomUUID');
  await expect(page.getByLabel('聊天记录').locator('strong')).toHaveText('Reply 1');
  expect(requests[0].conversation_id).toMatch(/^conv_[a-f0-9]{32}$/);
  expect(requests[0].model).toBe('test-model');
});

test('markdown images in model output render as links and are never fetched', async ({ page }) => {
  await mockAdmin(page);
  const fetched: string[] = [];
  page.on('request', (request) => { if (request.url().startsWith('https://attacker.example/')) fetched.push(request.url()); });
  await openChat(page);
  await send(page, 'Look ![leak](https://attacker.example/pixel.png?q=secret)');
  const history = page.getByLabel('聊天记录');
  await expect(history.locator('strong').last()).toHaveText('Reply 1');
  await expect(history.locator('img')).toHaveCount(0);
  const link = history.getByRole('link', { name: /leak/ });
  await expect(link).toHaveAttribute('href', 'https://attacker.example/pixel.png?q=secret');
  await expect(link).toHaveAttribute('rel', 'noopener noreferrer nofollow');
  await expect(link).toHaveAttribute('target', '_blank');
  expect(fetched).toEqual([]);
});

test('restricted workspace uses Auto despite its settings catalog and shows model evidence', async ({ page }) => {
  const { requests } = await mockAdmin(page, { restricted: true });
  await openChat(page);
  const workspace = page.getByRole('combobox', { name: '商业工作区' });
  await workspace.click();
  await page.getByRole('option', { name: /商业试用工作区/ }).click();
  const model = page.getByRole('combobox', { name: '模型', exact: true });
  await expect(model).toBeDisabled();
  await expect(model).toContainText('Auto');
  await expect(page.getByRole('note')).toContainText('仅支持 Auto');
  await send(page, 'Use the restricted workspace');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  expect(requests[0].model).toBe('auto');
  await expect(page.getByTestId('model-evidence').last()).toContainText('实际模型：Test model');
  await expect(page.getByTestId('model-evidence').last()).toContainText('Auto 分配');
  await page.reload();
  await expect(page.getByTestId('model-evidence').last()).toContainText('实际模型：Test model');
  await page.getByRole('button', { name: '新对话', exact: true }).click();
  await workspace.click();
  await page.getByRole('option', { name: /企业工作区/ }).click();
  await expect(model).toBeEnabled();
  await model.click();
  await page.getByRole('option', { name: 'Test model', exact: true }).click();
  await send(page, 'Use manual selection');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  expect(requests[1].model).toBe('test-model');
});
