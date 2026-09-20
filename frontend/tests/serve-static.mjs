import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../out', import.meta.url));
const types = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript',
  '.css': 'text/css',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
};

createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname);
    if (pathname === '/admin/__e2e/stream' && request.method === 'POST') {
      response.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
      let paragraph = 0;
      const timer = setInterval(() => {
        paragraph += 1;
        const content = `Paragraph ${paragraph}\n\n${'This is a streamed answer used to check reading and scrolling. '.repeat(10)}\n\n`;
        response.write(`data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\n\n`);
        if (paragraph === 20) { clearInterval(timer); response.end('data: [DONE]\n\n'); }
      }, 220);
      response.on('close', () => clearInterval(timer));
      return;
    }
    if (pathname !== '/admin' && !pathname.startsWith('/admin/')) throw new Error('not found');
    const relative = pathname.slice('/admin'.length).replace(/^\/+/, '') || 'index.html';
    const path = resolve(root, relative);
    if (!path.startsWith(root + sep)) throw new Error('not found');
    const data = await readFile(path);
    response.writeHead(200, { 'content-type': types[extname(path)] || 'application/octet-stream' });
    response.end(data);
  } catch {
    response.writeHead(404);
    response.end('Not found');
  }
}).listen(Number(process.env.E2E_PORT || 4173), '127.0.0.1');
