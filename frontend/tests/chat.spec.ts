import { expect, test, type Page } from '@playwright/test';
import type { ConversationDetail, ConversationMessage } from '../lib/services/admin/types';

async function mockAdmin(page: Page, truncated = false) {
  const requests: Array<{ conversation_id: string; prompt: string; stream: boolean }> = [];
  const conversations = new Map<string, ConversationDetail>();
  const responses: Record<string, unknown> = {
    '/admin/verify': { authenticated: true, password_configured: true, admin_enabled: true },
    '/admin/config': { config: { default_model: 'test-model', features: { use_web_search: false } }, models: [{ id: 'test-model', name: 'Test model' }] },
    '/admin/version': { version: 'test', default_model: 'test-model', model_count: 1 },
    '/healthz': { ok: true, session_ready: true },
    '/admin/accounts': { accounts: [] },
  };
  await page.route('**/*', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path in responses) return route.fulfill({ json: responses[path] });
    if (path === '/admin/events') {
      return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    }
    if (path === '/admin/conversations') return route.fulfill({ json: { items: [...conversations.values()] } });
    if (path.startsWith('/admin/conversations/')) {
      return route.fulfill({ json: { item: conversations.get(decodeURIComponent(path.split('/').pop()!)) } });
    }
    if (path === '/admin/test') {
      const input = route.request().postDataJSON();
      requests.push(input);
      const answer = `**Reply ${requests.length}**\n\n${input.prompt}`;
      const messages: ConversationMessage[] = [
        { id: `user-${requests.length}`, role: 'user', content: input.prompt, status: 'completed' },
        { id: `answer-${requests.length}`, role: 'assistant', content: answer, status: truncated ? 'failed' : 'completed' },
      ];
      const item: ConversationDetail = conversations.get(input.conversation_id) || { id: input.conversation_id, messages: [], model: 'test-model', account_email: 'test@example.com' };
      item.messages = [...(item.messages || []), ...messages];
      item.status = truncated ? 'failed' : 'completed';
      conversations.set(input.conversation_id, item);
      const parts = ['**Reply ', `${requests.length}**\n\n`, input.prompt];
      const body = parts.map((content) => `data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\r\n\r\n`).join('');
      return route.fulfill({
        contentType: 'text/event-stream',
        headers: { 'x-conversation-id': input.conversation_id },
        body: body + (truncated ? '' : 'data: [DONE]\r\n\r\n'),
      });
    }
    return route.continue();
  });
  return requests;
}

async function openChat(page: Page) {
  await page.goto('/admin');
  const chat = page.getByRole('button', { name: '\u804a\u5929', exact: true });
  if (page.viewportSize()!.width < 1024) {
    await page.getByRole('button', { name: '\u6253\u5f00\u5bfc\u822a' }).click();
  }
  await chat.click();
  await expect(page.getByRole('textbox', { name: '\u6d88\u606f', exact: true })).toBeEnabled();
}

test('two streamed turns preserve the conversation and restore history', async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const requests = await mockAdmin(page);
  await openChat(page);
  const history = page.getByLabel('\u804a\u5929\u8bb0\u5f55');
  for (const [turn, prompt] of ['First question', 'Follow-up question'].entries()) {
    await page.getByRole('textbox', { name: '\u6d88\u606f', exact: true }).fill(prompt);
    await page.getByRole('button', { name: '\u53d1\u9001', exact: true }).click();
    await expect(history.locator('strong').last()).toHaveText(`Reply ${turn + 1}`);
    await expect(page.getByRole('button', { name: '\u505c\u6b62', exact: true })).toHaveCount(0);
  }
  expect(requests).toHaveLength(2);
  expect(requests[0].stream).toBe(true);
  expect(requests[0].conversation_id).toBeTruthy();
  expect(requests[1].conversation_id).toBe(requests[0].conversation_id);
  await expect(history.locator('article')).toHaveCount(4);
  await expect(history.getByText('Follow-up question', { exact: true })).toHaveCount(2);
  await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }));
  const header = await page.locator('header').boundingBox();
  const heading = await page.getByRole('heading', { name: 'Notion AI', exact: true }).boundingBox();
  expect(heading!.y).toBeGreaterThanOrEqual(header!.y + header!.height);
  await page.screenshot({ path: testInfo.outputPath('chat.png'), fullPage: true });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await openChat(page);
  await expect(history.locator('article')).toHaveCount(4);
  await expect(history.locator('strong').last()).toHaveText('Reply 2');
  expect(errors).toEqual([]);
});

test('a truncated stream keeps partial text and reports failure', async ({ page }) => {
  await mockAdmin(page, true);
  await openChat(page);
  await page.getByRole('textbox', { name: '\u6d88\u606f', exact: true }).fill('Interrupted question');
  await page.getByRole('button', { name: '\u53d1\u9001', exact: true }).click();
  const history = page.getByLabel('\u804a\u5929\u8bb0\u5f55');
  await expect(history.locator('strong')).toHaveText('Reply 1');
  await expect(page.getByRole('status')).toContainText('\u8fde\u63a5\u4e2d\u65ad');
  await expect(history.getByText('\u672a\u5b8c\u6210', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: '\u505c\u6b62', exact: true })).toHaveCount(0);
});
