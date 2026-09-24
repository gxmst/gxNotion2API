import { expect, test, type Page } from '@playwright/test';
import type { ConversationDetail, ConversationMessage } from '../lib/services/admin/types';

const businessAccount = 'test@example.com';
const CONVERSATION_ID = 'conv_management';

interface Calls {
  renamed: Array<{ id: string; title: string }>;
  edited: Array<{ id: string; messageId: string; content: string }>;
  deleted: string[];
}

/**
 * A backend stub that keeps one conversation with one user turn and one
 * assistant turn, so rename, edit, delete and export can each be exercised
 * against the real request shapes the UI sends.
 */
async function mockAdmin(page: Page) {
  const calls: Calls = { renamed: [], edited: [], deleted: [] };
  const conversation: ConversationDetail = {
    id: CONVERSATION_ID,
    title: '原始标题',
    model: 'test-model',
    status: 'completed',
    account_email: businessAccount,
    space_id: 'business-trial',
    messages: [
      { id: 'm-user', role: 'user', content: '请总结这份文档', status: 'completed' },
      { id: 'm-step', role: 'step', step_type: 'agent-tool-result', content: 'searching the web', status: 'completed' },
      { id: 'm-answer', role: 'assistant', content: '这是总结结果。', status: 'completed' },
    ],
  };
  const responses: Record<string, unknown> = {
    '/admin/verify': { authenticated: true, password_configured: true, admin_enabled: true },
    '/admin/config': { config: { default_model: 'test-model', features: { use_web_search: false } }, models: [{ id: 'test-model', name: 'Test model' }] },
    '/admin/version': { version: 'test', default_model: 'test-model', model_count: 1 },
    '/healthz': { ok: true, session_ready: true },
    '/admin/accounts': { items: [{ email: businessAccount, workspaces: [
      { id: 'business-trial', name: '商业试用工作区', subscription_tier: 'business', plan_type: 'trial', eligible: true,
        model_capabilities: { mode: 'manual', models: [{ id: 'test-model', name: 'Test model', notion_model: 'test-codename', enabled: true }] } },
    ] }] },
  };
  await page.route('**/*', async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const method = request.method();
    if (path in responses) return route.fulfill({ json: responses[path] });
    if (path === '/admin/events') return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    if (path === '/admin/conversations') {
      const items = calls.deleted.includes(CONVERSATION_ID) ? [] : [conversation];
      return route.fulfill({ json: { items } });
    }
    const messageRoute = /^\/admin\/conversations\/([^/]+)\/messages\/([^/]+)$/.exec(path);
    if (messageRoute && method === 'PATCH') {
      const body = request.postDataJSON() as { content: string };
      calls.edited.push({ id: decodeURIComponent(messageRoute[1]), messageId: decodeURIComponent(messageRoute[2]), content: body.content });
      const target = conversation.messages!.find((message) => message.id === decodeURIComponent(messageRoute[2]));
      if (target) { target.content = body.content; target.edited_at = new Date().toISOString(); }
      return route.fulfill({ json: { success: true, item: conversation } });
    }
    if (path === `/admin/conversations/${CONVERSATION_ID}`) {
      if (method === 'DELETE') {
        calls.deleted.push(CONVERSATION_ID);
        return route.fulfill({ json: { success: true } });
      }
      if (method === 'PATCH') {
        const body = request.postDataJSON() as { title: string };
        calls.renamed.push({ id: CONVERSATION_ID, title: body.title });
        conversation.title = body.title;
        return route.fulfill({ json: { success: true, item: conversation } });
      }
      return route.fulfill({ json: { item: conversation } });
    }
    if (path === '/admin/test') {
      return route.fulfill({
        contentType: 'text/event-stream',
        headers: { 'x-conversation-id': CONVERSATION_ID },
        body: `data: ${JSON.stringify({ choices: [{ delta: { content: '好的' } }] })}\r\n\r\ndata: [DONE]\r\n\r\n`,
      });
    }
    return route.continue();
  });
  return { calls, conversation };
}

/**
 * The sidebar lives in an aside on desktop and in a drawer on narrow screens.
 * Opening a conversation closes that drawer, so the panel is fetched fresh for
 * every interaction rather than held on to; the drawer is always settled shut
 * before it is reopened, otherwise the handle goes stale mid-action.
 */
async function panel(page: Page) {
  if (page.viewportSize()!.width >= 1024) return page.locator('aside.chat-sidebar');
  const drawer = page.locator('.chat-mobile-sidebar');
  await expect(drawer).toBeHidden();
  await page.getByRole('button', { name: '打开导航', exact: true }).click();
  await expect(drawer).toBeVisible();
  return drawer;
}

async function openChatWithHistory(page: Page) {
  await page.goto('/admin');
  await expect(page.getByRole('textbox', { name: '消息', exact: true })).toBeEnabled();
  const list = await panel(page);
  await expect(list.locator('.chat-conversation')).toHaveCount(1);
  await list.locator('.chat-conversation-open').click();
  await expect(page.getByLabel('聊天记录')).toContainText('请总结这份文档');
  // The drawer closes itself once a conversation is picked; wait it out so the
  // next open is not racing the close.
  await expect(page.locator('.chat-mobile-sidebar')).toBeHidden();
}

test('a conversation can be renamed from the sidebar', async ({ page }) => {
  const { calls } = await mockAdmin(page);
  await openChatWithHistory(page);
  const list = await panel(page);
  const row = list.locator('.chat-conversation');
  await row.hover();
  await row.getByRole('button', { name: '重命名 原始标题' }).click();
  await list.getByRole('textbox', { name: '对话标题' }).fill('季度复盘');
  await list.getByRole('button', { name: '保存标题' }).click();
  await expect(row.locator('.chat-conversation-open span')).toHaveText('季度复盘');
  expect(calls.renamed).toEqual([{ id: CONVERSATION_ID, title: '季度复盘' }]);
});

test('renaming can be abandoned without a request', async ({ page }) => {
  const { calls } = await mockAdmin(page);
  await openChatWithHistory(page);
  const list = await panel(page);
  const row = list.locator('.chat-conversation');
  await row.hover();
  await row.getByRole('button', { name: '重命名 原始标题' }).click();
  await list.getByRole('textbox', { name: '对话标题' }).fill('不该保存');
  await list.getByRole('button', { name: '取消重命名' }).click();
  await expect(row.locator('.chat-conversation-open span')).toHaveText('原始标题');
  expect(calls.renamed).toEqual([]);
});

test('deleting asks first and then removes the conversation', async ({ page }) => {
  const { calls } = await mockAdmin(page);
  await openChatWithHistory(page);
  const list = await panel(page);
  await list.locator('.chat-conversation').hover();
  await list.getByRole('button', { name: '删除 原始标题' }).click();
  // Dismissing the confirmation must leave the conversation alone.
  expect(calls.deleted).toEqual([]);
  await expect(list.locator('.chat-conversation')).toHaveCount(1);
  page.on('dialog', (dialog) => dialog.accept());
  await list.locator('.chat-conversation').hover();
  await list.getByRole('button', { name: '删除 原始标题' }).click();
  await expect(list.locator('.chat-conversation')).toHaveCount(0);
  expect(calls.deleted).toEqual([CONVERSATION_ID]);
});

test('exporting writes the transcript to a markdown file', async ({ page }) => {
  await mockAdmin(page);
  await openChatWithHistory(page);
  const list = await panel(page);
  const row = list.locator('.chat-conversation');
  await row.hover();
  const download = await Promise.all([
    page.waitForEvent('download'),
    list.getByRole('button', { name: '导出 原始标题' }).click(),
  ]).then(([event]) => event);
  expect(download.suggestedFilename()).toBe('原始标题.md');
  const stream = await download.createReadStream();
  const text = await new Promise<string>((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on('data', (chunk: Buffer) => chunks.push(chunk));
    stream.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    stream.on('error', reject);
  });
  expect(text).toContain('# 原始标题');
  expect(text).toContain('## 用户');
  expect(text).toContain('请总结这份文档');
  expect(text).toContain('## Notion AI');
  expect(text).toContain('这是总结结果。');
  // A process step is exported as context rather than as a turn of its own.
  expect(text).toContain('> 过程 · 调用工具：searching the web');
  expect(text).not.toContain('## 用户\nsearching the web');
});

test('both a user message and an assistant message can be edited', async ({ page }) => {
  const { calls } = await mockAdmin(page);
  await openChatWithHistory(page);
  const transcript = page.getByLabel('聊天记录');
  await transcript.locator('article').nth(0).getByRole('button', { name: '编辑消息' }).click();
  const editor = page.getByRole('textbox', { name: '编辑消息内容' });
  await editor.fill('请总结这份文档（补充：只看结论）');
  await page.getByRole('button', { name: '保存修改' }).click();
  await expect(transcript).toContainText('只看结论');
  await expect(transcript.locator('.chat-edited')).toHaveCount(1);

  await transcript.locator('article').nth(1).getByRole('button', { name: '编辑消息' }).click();
  await page.getByRole('textbox', { name: '编辑消息内容' }).fill('这是修改后的总结结果。');
  await page.getByRole('button', { name: '保存修改' }).click();
  await expect(transcript).toContainText('这是修改后的总结结果。');
  expect(calls.edited).toEqual([
    { id: CONVERSATION_ID, messageId: 'm-user', content: '请总结这份文档（补充：只看结论）' },
    { id: CONVERSATION_ID, messageId: 'm-answer', content: '这是修改后的总结结果。' },
  ]);
});

test('editing can be cancelled and empty text is refused', async ({ page }) => {
  const { calls } = await mockAdmin(page);
  await openChatWithHistory(page);
  const transcript = page.getByLabel('聊天记录');
  const first = transcript.locator('article').nth(0);
  await first.getByRole('button', { name: '编辑消息' }).click();
  await page.getByRole('textbox', { name: '编辑消息内容' }).fill('   ');
  await expect(page.getByRole('button', { name: '保存修改' })).toBeDisabled();
  await page.getByRole('button', { name: '取消修改' }).click();
  await expect(transcript).toContainText('请总结这份文档');
  expect(calls.edited).toEqual([]);
});

test('row actions stay hidden until the row is hovered', async ({ page }) => {
  await mockAdmin(page);
  await openChatWithHistory(page);
  const list = await panel(page);
  const actions = list.locator('.chat-conversation-actions');
  // Park the pointer away from the list: the click that opened the conversation
  // left it over the row, and a hover would mask what is being asserted.
  await page.mouse.move(0, 0);
  // Opening a conversation leaves its row focused; the buttons must still hide,
  // otherwise a mouse click pins them open for the rest of the session.
  await expect(actions).toHaveCSS('opacity', '0');
  await list.locator('.chat-conversation').hover();
  await expect(actions).toHaveCSS('opacity', '1');
});

test('process steps render as slim markers and token estimates are shown', async ({ page }) => {
  await mockAdmin(page);
  await openChatWithHistory(page);
  const transcript = page.getByLabel('聊天记录');
  await expect(transcript.locator('.chat-step')).toHaveCount(1);
  await expect(transcript.locator('.chat-step-label')).toHaveText('调用工具');
  // The step is context, not a chat turn, so it must not add a bubble.
  await expect(transcript.locator('article')).toHaveCount(2);
  await expect(page.locator('.chat-token-total')).toContainText('估算');
});
