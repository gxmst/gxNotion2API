import { expect, test, type Page } from '@playwright/test';

const conversationID = 'markdown-regression';
const markdown = [
  '## 中文 Markdown 验证',
  '**为什么需要发布：**后面紧接中文，不额外插入空格。',
  '- **时间点很敏感。**这句应当加粗。\n- **“中文引号”**也支持，包含 *斜体* 和 ~~删除线~~。',
  '1. 第一项\n2. 第二项',
  '- [x] 已验证\n- [ ] 待验证',
  '> 引用说明',
  '| 项目 | 状态 |\n| --- | ---: |\n| **中文：**表格 | 正常 |',
  '`**代码里的中文：**保留星号`',
  '```js\nconst value = "**代码块：**保留原文";\n' + '// ' + 'long-code-line '.repeat(20) + '\n```',
  '\\*\\*转义星号\\*\\*',
  '[安全链接](https://example.com/docs) 和 [危险链接](javascript:alert%281%29)',
  '![外部图片](https://example.invalid/markdown.png)',
  '<script>window.markdownExecuted = true</script>',
].join('\n\n');

async function fixture(page: Page, restore = true, answer = markdown) {
  const externalRequests: string[] = [];
  page.on('pageerror', error => { throw error; });
  page.on('request', request => {
    if (request.url().startsWith('https://example.invalid/')) externalRequests.push(request.url());
  });
  if (restore) await page.addInitScript(id => {
    sessionStorage.setItem('notion2api-chat-session', JSON.stringify({ conversationID: id }));
  }, conversationID);
  const conversation = {
    id: conversationID, title: 'Markdown 验证', model: 'auto', status: 'completed',
    account_email: 'test@example.com', space_id: 'workspace',
    messages: [
      { id: 'question', role: 'user', content: '**用户输入保留原文。**', status: 'completed' },
      { id: 'answer', role: 'assistant', content: answer, status: 'completed' },
    ],
  };
  const responses: Record<string, unknown> = {
    '/admin/verify': { authenticated: true, password_configured: true, admin_enabled: true },
    '/admin/config': { config: { default_model: 'auto', features: {} }, models: [{ id: 'auto', name: 'Auto' }] },
    '/admin/version': { version: 'test', default_model: 'auto' },
    '/healthz': { ok: true, session_ready: true },
    '/admin/accounts': { items: [{ email: 'test@example.com', workspaces: [{ id: 'workspace', name: '测试工作区', eligible: true, plan_type: 'business', model_capabilities: { mode: 'auto_only', models: [] } }] }] },
    '/admin/accounts/ai-usage': { accounts: [] },
    '/admin/accounts/ai-usage/workspace': {},
    '/admin/conversations': { items: [conversation] },
  };
  await page.route('**/*', async route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/admin/events') return route.fulfill({ contentType: 'text/event-stream', body: 'event: admin.ready\ndata: {}\n\n' });
    if (path.startsWith('/admin/conversations/')) return route.fulfill({ json: { item: conversation } });
    if (path in responses) return route.fulfill({ json: responses[path] });
    return route.continue();
  });
  return externalRequests;
}

for (const tab of ['', 'tester', 'conversations']) {
  test(`Chinese Markdown renders consistently in ${tab || 'chat'}`, async ({ page }, testInfo) => {
    const externalRequests = await fixture(page);
    await page.goto('/admin' + (tab ? '?tab=' + tab : ''));
    const reply = page.locator('.message-markdown:visible');
    await expect(reply).toHaveCount(1);
    await expect(reply.getByRole('heading', { name: '中文 Markdown 验证', level: 2 })).toBeAttached();
    await expect(reply.locator('strong')).toHaveText(['为什么需要发布：', '时间点很敏感。', '“中文引号”', '中文：']);
    await expect(reply.locator('em')).toHaveText('斜体');
    await expect(reply.locator('del')).toHaveText('删除线');
    await expect(reply.locator('blockquote')).toHaveText('引用说明');
    await expect(reply.locator('ol > li')).toHaveCount(2);
    await expect(reply.getByRole('checkbox').first()).toBeChecked();
    await expect(reply.getByRole('checkbox').first()).toBeDisabled();
    await expect(reply.locator('td').last()).toHaveCSS('text-align', 'right');
    await expect(reply.locator('pre code')).toContainText('"**代码块：**保留原文"');
    await expect(reply.locator('p > code')).toHaveText('**代码里的中文：**保留星号');
    await expect(reply).toContainText('**转义星号**');
    await expect(reply.locator('script, img')).toHaveCount(0);
    await expect(reply.getByRole('link', { name: '安全链接' })).toHaveAttribute('rel', /noopener/);
    expect(await reply.getByRole('link', { name: '危险链接' }).getAttribute('href')).not.toMatch(/^javascript:/);
    expect(await page.evaluate(() => Reflect.get(window, 'markdownExecuted'))).toBeUndefined();
    expect(externalRequests).toEqual([]);
    await expect(page.getByText('**用户输入保留原文。**', { exact: true }).first()).toBeAttached();
    for (const theme of ['light', 'dark']) {
      await page.evaluate(value => { document.documentElement.classList.remove('light', 'dark'); document.documentElement.classList.add(value); }, theme);
      await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await reply.getByRole('heading').scrollIntoViewIfNeeded();
      await page.screenshot({ path: testInfo.outputPath(`${tab || 'chat'}-${theme}.png`) });
    }
  });
}

test('Chinese emphasis completes during streaming and exports the original Markdown', async ({ page }) => {
  await fixture(page, false, '**时间点很敏感。**后面紧接中文。');
  await page.route('**/admin/test', route => route.continue({ url: new URL('/admin/__e2e/markdown-stream', route.request().url()).href }));
  await page.goto('/admin');
  await page.getByRole('textbox', { name: '消息', exact: true }).fill('验证流式 Markdown');
  await page.getByRole('button', { name: '发送', exact: true }).click();
  const reply = page.locator('.message-markdown');
  await expect(reply).toContainText('**时间点很敏感。');
  await expect(reply.locator('strong')).toHaveCount(0);
  await expect(reply.locator('strong')).toHaveText('时间点很敏感。');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toBeVisible();
  await expect(reply).toContainText('后面紧接中文。');
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  await expect(reply).not.toContainText('**');
  const downloadPromise = page.waitForEvent('download');
  await page.getByRole('button', { name: '导出 Markdown', exact: true }).click();
  const download = await downloadPromise;
  const chunks: Buffer[] = [];
  for await (const chunk of await download.createReadStream()) chunks.push(chunk);
  expect(Buffer.concat(chunks).toString('utf8')).toContain('**时间点很敏感。**后面紧接中文。');
});
