import type { AttachmentInput } from '@/lib/services/admin/types';
import { EventSourceParserStream } from 'eventsource-parser/stream';

const API_BASE = (process.env.NEXT_PUBLIC_BACKEND_BASE_URL || '').replace(/\/$/, '');

function buildURL(path: string): string {
  return `${API_BASE}${path}`;
}

export function buildEventStreamURL(path: string): string {
  return buildURL(path);
}

function summarizeHTMLText(raw: string): string {
  const text = String(raw || '').trim();
  const lower = text.toLowerCase();
  if (!(lower.startsWith('<!doctype html') || lower.startsWith('<html') || lower.includes('<title'))) {
    return '';
  }
  const titleMatch = text.match(/<title[^>]*>([\s\S]*?)<\/title>/i);
  const h1Match = text.match(/<h1[^>]*>([\s\S]*?)<\/h1>/i);
  const strip = (value: string) => value.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
  const title = strip(titleMatch?.[1] || '');
  const h1 = strip(h1Match?.[1] || '');
  if (title && h1 && title !== h1) return `上游返回 HTML 错误页: ${title} | ${h1}`;
  if (title) return `上游返回 HTML 错误页: ${title}`;
  if (h1) return `上游返回 HTML 错误页: ${h1}`;
  return '上游返回了 HTML 错误页';
}

export class ApiError extends Error {
  constructor(message: string, public readonly status: number) {
    super(message);
    this.name = 'ApiError';
  }
}

export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers || {});
  if (init?.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  const response = await fetch(buildURL(path), {
    ...init,
    headers,
    credentials: 'include',
  });
  const contentType = response.headers.get('content-type') || '';
  const hitNation2API = response.headers.get('x-notion2api') === '1';
  const payload = contentType.includes('application/json') ? await response.json() : await response.text();

  if (!response.ok) {
    const htmlSummary = typeof payload === 'string' ? summarizeHTMLText(payload) : '';
    if (htmlSummary && !hitNation2API) {
      throw new ApiError(`当前响应未命中 nation2api（status ${response.status} ${response.statusText}，url ${response.url}），而是前置代理/反代返回的 HTML 错误页: ${htmlSummary.replace(/^上游返回 HTML 错误页:\s*/, '')}`, response.status);
    }
    if (typeof payload === 'object' && payload !== null) {
      const detail = (payload as { detail?: string; error?: { message?: string } }).detail;
      const message = (payload as { detail?: string; error?: { message?: string } }).error?.message;
      throw new ApiError(detail || message || `${response.status} ${response.statusText}`, response.status);
    }
    throw new ApiError(htmlSummary || String(payload || `${response.status} ${response.statusText}`), response.status);
  }

  return payload as T;
}

export async function apiEventStream(path: string, payload: unknown, onEvent: (data: string) => void, signal: AbortSignal): Promise<Headers> {
  const response = await fetch(buildURL(path), {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload), credentials: 'include', signal,
  });
  if (!response.ok || !response.body || !response.headers.get('content-type')?.includes('text/event-stream')) {
    const text = await response.text();
    let message = summarizeHTMLText(text) || `${response.status} ${response.statusText}`;
    try {
      const error = JSON.parse(text);
      message = error.detail || error.error?.message || message;
    } catch { /* Non-JSON responses use the HTTP status or HTML title. */ }
    throw new Error(message);
  }
  const reader = response.body.pipeThrough(new TextDecoderStream()).pipeThrough(new EventSourceParserStream()).getReader();
  let done = false;
  try {
    while (true) {
      const item = await reader.read();
      if (item.done) break;
      if (item.value.data === '[DONE]') { done = true; break; }
      onEvent(item.value.data);
    }
    if (!done) throw new Error('连接中断，回答尚未完成');
  } finally {
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
  return response.headers;
}

export async function readFilesAsAttachments(files: File[]): Promise<AttachmentInput[]> {
  return Promise.all(
    files.map(
      (file) =>
        new Promise<AttachmentInput>((resolve, reject) => {
          const reader = new FileReader();
          reader.onload = () =>
            resolve({
              type: 'attachment',
              name: file.name,
              content_type: file.type,
              data: reader.result,
            });
          reader.onerror = () => reject(reader.error || new Error('读取文件失败'));
          reader.readAsDataURL(file);
        }),
    ),
  );
}

export async function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const area = document.createElement('textarea');
  area.value = text;
  area.setAttribute('readonly', '');
  area.style.position = 'fixed';
  area.style.opacity = '0';
  document.body.appendChild(area);
  area.select();
  try {
    if (!document.execCommand('copy')) throw new Error('复制失败');
  } finally {
    area.remove();
  }
}
