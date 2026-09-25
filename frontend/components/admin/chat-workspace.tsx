'use client';

import { useEffect, useRef, useState } from 'react';
import { ArrowDown, ArrowUp, Check, Copy, Download, FileText, Gauge, Globe2, ImageIcon, KeyRound, LoaderCircle, MessageSquare, Moon, PanelLeftClose, PanelLeftOpen, Paperclip, Pencil, Plus, RefreshCw, Search, Settings2, Sparkles, Square, Sun, Trash2, X } from 'lucide-react';
import { useTheme } from 'next-themes';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { toast } from 'sonner';
import { AdminService } from '@/lib/services/admin/admin.service';
import { ModelEvidence } from '@/components/admin/model-evidence';
import { safeMarkdownComponents } from '@/components/admin/safe-markdown';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Dialog, DialogContent, DialogTitle } from '@/components/ui/dialog';
import { copyText, readFilesAsAttachments } from '@/lib/services/core/api-client';
import type { AccountItem, AIUsageReport, AIUsageRateLimitWindow, ChatRunInput, ChatRunResult, ConversationDetailPayload, ConversationMessage, ConversationSummary, ModelItem, TabKey } from '@/lib/services/admin/types';

const SESSION_KEY = 'notion2api-chat-session';
// How often a completed turn may force a fresh quota read. The server caps
// forced reads at 30s globally, so this only keeps a busy client from spending
// the whole allowance on the indicator.
const QUOTA_FORCE_MIN_INTERVAL_MS = 60_000;

// Attachments travel inside the JSON request body as base64 data URLs, so the
// limit a browser actually hits is the server's *body* cap, not the larger
// per-attachment cap it also enforces. A 4 MiB body leaves roughly 3 MiB of raw
// file once base64 (4/3) and the surrounding JSON are accounted for. Guarding
// here turns a late, opaque server rejection into an immediate, specific one.
const MAX_REQUEST_BODY_BYTES = 4 * 1024 * 1024;
const JSON_ENVELOPE_BYTES = 2048;
const BASE64_PREFIX_BYTES = 64;

function encodedAttachmentBytes(file: File): number {
  return Math.ceil((file.size * 4) / 3) + BASE64_PREFIX_BYTES;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// Notion-shared files report a content type, but a pasted screenshot may only
// carry a name, so fall back to the extension.
function attachmentIsImage(file: { content_type?: string; contentType?: string; name?: string }): boolean {
  const type = (file.content_type || file.contentType || '').toLowerCase();
  if (type.startsWith('image/')) return true;
  return /\.(png|jpe?g|gif|webp|avif|bmp|svg)$/i.test(file.name || '');
}

// Upstream never reports a token count for a bridged turn, so the numbers shown
// are a local estimate: CJK characters land near one token each while Latin text
// runs closer to four characters per token. Every place it appears is labelled
// as an estimate, so it is never read as billing data.
function isCJKIdeograph(code: number): boolean {
  return (code >= 0x2e80 && code <= 0x9fff) || (code >= 0xf900 && code <= 0xfaff)
    || (code >= 0xac00 && code <= 0xd7af) || (code >= 0xff00 && code <= 0xffef);
}

function estimateTokens(text?: string): number {
  const value = text || '';
  if (!value) return 0;
  let cjk = 0;
  let other = 0;
  for (const char of value) {
    if (isCJKIdeograph(char.codePointAt(0) || 0)) cjk += 1;
    else other += 1;
  }
  return cjk + Math.ceil(other / 4);
}

function formatTokens(count: number): string {
  return count >= 10000 ? `${(count / 1000).toFixed(1)}k` : String(count);
}

// Step names come from upstream verbatim, so they are mapped onto a readable
// label when the family is recognisable and printed as-is otherwise. An
// unknown step still renders with its own name rather than as a blank row.
const STEP_LABELS: Array<[RegExp, string]> = [
  [/search/, '联网搜索'],
  [/tool/, '调用工具'],
  [/think|reason/, '思考'],
  [/read|file|attach/, '读取文件'],
  [/title/, '生成标题'],
];

function stepLabel(raw?: string): string {
  const type = (raw || '').trim();
  if (!type) return '处理步骤';
  const lower = type.toLowerCase();
  for (const [pattern, label] of STEP_LABELS) if (pattern.test(lower)) return label;
  return type;
}

function conversationMarkdown(title: string, messages: ConversationMessage[], meta: { model?: string; account?: string } = {}): string {
  const lines: string[] = [`# ${title || '对话'}`, ''];
  const facts = [meta.model ? `模型：${meta.model}` : '', meta.account ? `账号：${meta.account}` : '', `导出时间：${new Date().toLocaleString()}`].filter(Boolean);
  lines.push(`> ${facts.join(' · ')}`, '');
  for (const message of messages) {
    const role = (message.role || '').toLowerCase();
    if (role === 'step') {
      lines.push(`> 过程 · ${stepLabel(message.step_type)}${message.content ? `：${message.content}` : ''}`);
      continue;
    }
    lines.push(role === 'user' ? '## 用户' : '## Notion AI', '');
    if (message.content) lines.push(message.content, '');
    if (message.attachments?.length) {
      lines.push('附件：');
      message.attachments.forEach((file, index) => {
        const name = file.name || `附件 ${index + 1}`;
        lines.push(file.url ? `- [${name}](${file.url})` : `- ${name}`);
      });
      lines.push('');
    }
    if (message.edited_at) lines.push('_（此条内容已被手动编辑）_', '');
  }
  return lines.join('\n').replace(/\n{3,}/g, '\n\n').trimEnd() + '\n';
}

function safeFileName(name: string): string {
  const cleaned = (name || '').replace(/[\\/:*?"<>|\u0000-\u001f]/g, ' ').replace(/\s+/g, ' ').trim();
  return (cleaned || 'conversation').slice(0, 80);
}

function downloadTextFile(filename: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/markdown;charset=utf-8' }));
  const anchor = document.createElement('a');
  anchor.href = url; anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  // Revoked on the next tick so the browser has already taken the URL.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

function newID() {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID().replace(/-/g, '');
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) => byte.toString(16).padStart(2, '0')).join('');
}
const targetID = (email: string, workspace: string) => JSON.stringify([email, workspace]);

// The window length is Notion's, so its own name is kept and only translated
// into a readable form ("6h" -> "6 小时", "billing_period" -> "账单周期").
function quotaWindowLabel(raw: string | undefined, fallback: string): string {
  const value = (raw || '').trim();
  if (!value) return fallback;
  if (value === 'billing_period') return '账单周期';
  const hours = /^(\d+)h$/.exec(value);
  if (hours) return `${hours[1]} 小时`;
  const days = /^(\d+)d$/.exec(value);
  if (days) return `${days[1]} 天`;
  return value;
}

// A remaining percentage is only derived from confirmed counters: an absent or
// zero limit means upstream did not state one, not that nothing is left.
function quotaRemainingPercent(window?: AIUsageRateLimitWindow): number | null {
  if (!window || !Number.isFinite(window.limit) || window.limit <= 0) return null;
  const used = Number.isFinite(window.used) ? window.used : 0;
  return Math.max(0, Math.min(100, Math.round(((window.limit - used) / window.limit) * 100)));
}

function quotaTone(percent: number): 'ok' | 'warn' | 'low' {
  if (percent <= 15) return 'low';
  if (percent <= 40) return 'warn';
  return 'ok';
}

function formatQuotaPeriodEnd(ms?: number): string {
  if (!ms || !Number.isFinite(ms)) return '';
  const date = new Date(ms);
  if (Number.isNaN(date.getTime())) return '';
  return `${date.getMonth() + 1} 月 ${date.getDate()} 日`;
}

// Counters are shown as upstream reported them. A missing limit is printed as
// unknown rather than as zero, which would read as "nothing left".
function formatQuotaAmount(window: AIUsageRateLimitWindow): string {
  const round = (value: number) => (Number.isInteger(value) ? String(value) : value.toFixed(2).replace(/0+$/, '').replace(/\.$/, ''));
  const used = Number.isFinite(window.used) ? round(window.used) : '?';
  const limit = Number.isFinite(window.limit) && window.limit > 0 ? round(window.limit) : '未知';
  const periodEnd = formatQuotaPeriodEnd(window.period_end_ms);
  return `已用 ${used} / ${limit}${periodEnd ? ` · 至 ${periodEnd}` : ''}`;
}

export function ChatWorkspace({ models, defaultModel, defaultWebSearch, initialConversationID, onResumeHandled, onLoad, onRun, conversations, accounts, onNavigate, onDeleteConversation, onRefreshConversations, visible }: {
  models: ModelItem[];
  defaultModel?: string;
  defaultWebSearch: boolean;
  initialConversationID?: string;
  onResumeHandled: () => void;
  onLoad: (id: string) => Promise<ConversationDetailPayload>;
  onRun: (payload: ChatRunInput, onDelta: (text: string) => void, signal: AbortSignal) => Promise<ChatRunResult>;
  conversations: ConversationSummary[];
  accounts: AccountItem[];
  onNavigate: (tab: TabKey) => void;
  /** Deletes a conversation and refreshes the list; owned by the console. */
  onDeleteConversation: (id: string) => Promise<unknown>;
  /** Re-reads the sidebar list after a rename or a delete. */
  onRefreshConversations: () => Promise<unknown>;
  visible: boolean;
}) {
  const [prompt, setPrompt] = useState('');
  const [model, setModel] = useState(defaultModel || models[0]?.id || 'auto');
  const [useWebSearch, setUseWebSearch] = useState(defaultWebSearch);
  const [conversationID, setConversationID] = useState('');
  // Remote-only transcripts live in Notion, not in this bridge's store, so
  // rename and per-message edit have nothing local to write to. Their controls
  // are hidden rather than offered and then failing on save.
  const [remoteOnly, setRemoteOnly] = useState(false);
  const [messages, setMessages] = useState<ConversationMessage[]>([]);
  const [files, setFiles] = useState<File[]>([]);
  const [dragging, setDragging] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadFailed, setLoadFailed] = useState(false);
  const [remoteRunning, setRemoteRunning] = useState(false);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState('');
  const [target, setTarget] = useState('auto');
  const [owner, setOwner] = useState('');
  const [title, setTitle] = useState('新对话');
  const [filter, setFilter] = useState('');
  const [mobileOpen, setMobileOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [atBottom, setAtBottom] = useState(true);
  const [copied, setCopied] = useState('');
  // Inline rename in the sidebar, and per-message editing in the transcript.
  const [renamingID, setRenamingID] = useState('');
  const [renameValue, setRenameValue] = useState('');
  const [editingID, setEditingID] = useState('');
  const [editValue, setEditValue] = useState('');
  // One shared busy flag would freeze every row while a single request is in
  // flight, so the pending action is tracked per conversation.
  const [busyID, setBusyID] = useState('');
  const [savingEdit, setSavingEdit] = useState(false);
  const [quota, setQuota] = useState<AIUsageReport | null>(null);
  // The workspace the stored report was fetched for. A report is only rendered
  // while it still matches the workspace the next turn would use, so switching
  // workspaces never shows one workspace's numbers under another's name.
  const [quotaKey, setQuotaKey] = useState('');
  const [quotaRows, setQuotaRows] = useState<AIUsageReport[] | null>(null);
  const [quotaLoading, setQuotaLoading] = useState(false);
  // The auto-mode list and the single-workspace read are separate requests; a
  // shared flag let the faster list clear the flag while the workspace read was
  // still in flight, which briefly rendered "no windows" for a pending read.
  const [quotaRowsLoading, setQuotaRowsLoading] = useState(false);
  const [quotaOpen, setQuotaOpen] = useState(false);
  const [quotaError, setQuotaError] = useState('');
  const [quotaRevision, setQuotaRevision] = useState(0);
  const quotaForcedAt = useRef(0);
  const quotaForceNext = useRef(false);
  const abortRef = useRef<AbortController | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  // dragenter/dragleave fire for every child element, so a depth counter is what
  // keeps the drop overlay from flickering as the pointer moves inside it.
  const dragDepth = useRef(0);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const historyRef = useRef<HTMLDivElement>(null);
  const shellRef = useRef<HTMLDivElement>(null);
  const mounted = useRef(false);
  const loadRevision = useRef(0);
  // Mirrors conversationID so an in-flight save can tell whether the operator
  // moved to another conversation before its response landed.
  const activeConversationRef = useRef('');
  const followBottom = useRef(true);
  const propsRef = useRef({ onLoad, onResumeHandled, onDeleteConversation, onRefreshConversations });
  propsRef.current = { onLoad, onResumeHandled, onDeleteConversation, onRefreshConversations };
  const { resolvedTheme, setTheme } = useTheme();

  const allTargets = accounts.flatMap((account) => (account.workspaces || [])
    .map((workspace) => ({ id: targetID(account.email || '', workspace.id), email: account.email || '', workspace: workspace.id,
      name: workspace.name || workspace.id, tier: workspace.subscription_tier || workspace.plan_type || '套餐待确认',
      capability: workspace.model_capabilities,
      eligible: !account.disabled && workspace.eligible })));
  const targets = allTargets.filter((workspace) => workspace.eligible);
  const selectedTarget = targets.find((item) => item.id === target);
  const boundTarget = allTargets.find((item) => item.id === target);
  const modelTargets = target === 'auto' ? targets : boundTarget ? [boundTarget] : [];
  const availableModels: ModelItem[] = [{ id: 'auto', name: 'Auto' }, ...Array.from(new Map(modelTargets.flatMap((item) => item.capability?.mode === 'manual' ? item.capability.models || [] : [])
    .filter((item) => item.enabled !== false && item.id !== 'auto' && models.find((model) => model.id === item.id)?.enabled !== false).map((item) => [item.id, item])).values())];
  const modelCatalog = [...models, ...allTargets.flatMap((item) => [...(item.capability?.models || []), ...(item.capability?.catalog || [])])];
  const modelChoiceKey = availableModels.map((item) => item.id).join('|');
  const autoOnly = boundTarget?.capability?.mode === 'auto_only';
  const history = conversations.filter((item) => (item.title || item.preview || item.request_prompt || '新对话').toLowerCase().includes(filter.toLowerCase()));
  // Quota is per workspace, so the indicator follows the workspace this turn
  // will actually use: the bound one when a conversation pins it, the
  // explicitly selected one otherwise. In auto mode there is no single
  // workspace until the first turn binds one.
  const effectiveWorkspace = target === 'auto' ? null : boundTarget ?? null;
  const effectiveWorkspaceKey = effectiveWorkspace ? effectiveWorkspace.email + '\u0000' + effectiveWorkspace.workspace : '';
  const activeQuota = effectiveWorkspaceKey !== '' && quotaKey === effectiveWorkspaceKey ? quota : null;
  const shortWindow = activeQuota?.usage?.rate_limit?.short;
  const longWindow = activeQuota?.usage?.rate_limit?.long;
  const primaryWindow = shortWindow ?? longWindow;
  const primaryPercent = quotaRemainingPercent(primaryWindow);

  async function loadQuotaRows(refresh = false) {
    setQuotaRowsLoading(true); setQuotaError('');
    try {
      const { accounts } = await AdminService.getAIUsage(refresh);
      const eligible = new Set(targets.map((item) => item.workspace));
      setQuotaRows(accounts.filter((row) => row.space_id && eligible.has(row.space_id)));
    } catch (cause) {
      setQuotaError(cause instanceof Error ? cause.message : '额度读取失败');
    } finally { setQuotaRowsLoading(false); }
  }

  // A fresh read is asked for at most once a minute; the server also caps
  // forced reads, so a chatty session cannot turn the indicator into a stream
  // of upstream calls.
  function refreshQuota() {
    if (effectiveWorkspace) {
      if (Date.now() - quotaForcedAt.current >= QUOTA_FORCE_MIN_INTERVAL_MS) {
        quotaForcedAt.current = Date.now();
        quotaForceNext.current = true;
      }
      setQuotaRevision((value) => value + 1);
      return;
    }
    // Auto mode has no single workspace, so the pool-wide list is only worth
    // reading to repaint a panel that is already open. Refreshing it after
    // every turn would scan every workspace for an indicator nobody is
    // looking at; opening the panel reads it on demand instead.
    if (quotaOpen) void loadQuotaRows(true);
  }

  function toggleQuota() {
    const next = !quotaOpen;
    setQuotaOpen(next);
    if (next && !effectiveWorkspace) void loadQuotaRows();
  }

  async function openConversation(id: string, draft = '') {
    if (abortRef.current) { toast.info('请先停止当前生成，再切换对话'); return; }
    const revision = ++loadRevision.current;
    setLoading(true); setError(''); setLoadFailed(false); setRemoteRunning(false); setQuotaOpen(false);
    setConversationID(id); setMessages([]); setFiles([]); setPrompt(draft); setOwner(''); setTarget('auto');
    setTitle('加载对话…');
    setMobileOpen(false); followBottom.current = true;
    cancelRename(); cancelEdit();
    try {
      const { item } = await propsRef.current.onLoad(id);
      if (!mounted.current || revision !== loadRevision.current) return;
      setMessages(item.messages || []); setOwner(item.account_email || '');
      setTitle(item.title || item.request_prompt?.slice(0, 60) || '对话');
      setRemoteOnly(Boolean(item.remote_only));
      if (item.account_email && item.space_id) setTarget(targetID(item.account_email, item.space_id));
      if (item.model && models.some((model) => model.id === item.model)) setModel(item.model);
      setRemoteRunning(item.status === 'running' || item.status === 'queued');
    } catch (cause) {
      if (mounted.current && revision === loadRevision.current) { setLoadFailed(true); setError(cause instanceof Error ? cause.message : '会话加载失败'); }
    } finally { if (mounted.current && revision === loadRevision.current) setLoading(false); }
  }

  useEffect(() => {
    mounted.current = true;
    let saved: { conversationID?: string; prompt?: string; model?: string; useWebSearch?: boolean; target?: string } = {};
    try {
      const value: unknown = JSON.parse(sessionStorage.getItem(SESSION_KEY) || localStorage.getItem(SESSION_KEY) || '{}');
      if (value && typeof value === 'object' && !Array.isArray(value)) saved = value;
    } catch { /* Optional storage. */ }
    if (typeof saved.model === 'string' && models.some((item) => item.id === saved.model)) setModel(saved.model);
    if (typeof saved.useWebSearch === 'boolean') setUseWebSearch(saved.useWebSearch);
    if (typeof saved.target === 'string') setTarget(saved.target);
    const draft = typeof saved.prompt === 'string' ? saved.prompt : '';
    if (typeof saved.conversationID === 'string' && saved.conversationID) void openConversation(saved.conversationID, draft);
    else { setPrompt(draft); setLoading(false); }
    return () => { mounted.current = false; loadRevision.current++; abortRef.current?.abort(); };
    // The workspace stays mounted while visiting management pages.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (!initialConversationID) return;
    if (initialConversationID !== conversationID) void openConversation(initialConversationID);
    propsRef.current.onResumeHandled();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initialConversationID]);

  useEffect(() => {
    if (!running && !loading && !availableModels.some((item) => item.id === model)) setModel('auto');
    // Model choices follow the current workspace capability snapshot.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelChoiceKey, model, running, loading]);

  useEffect(() => {
    if (!visible || !effectiveWorkspaceKey) { setQuota(null); return; }
    const [email, workspace] = effectiveWorkspaceKey.split('\u0000');
    // Without an email the account cannot be named, and the server would fall
    // back to whichever account happens to be active. Say so instead of asking.
    if (!email) { setQuota(null); setQuotaLoading(false); setQuotaError('该工作区所属账号缺少邮箱，无法读取额度'); return; }
    const force = quotaForceNext.current;
    quotaForceNext.current = false;
    let cancelled = false;
    setQuotaLoading(true); setQuotaError('');
    AdminService.getWorkspaceAIUsage(email, workspace, force)
      .then(({ report }) => {
        if (cancelled) return;
        // The server caps forced reads globally and silently serves its cache
        // when someone else refreshed moments ago. A declined refresh must not
        // count against the client's own interval, or the next attempt would be
        // throttled by a read that never happened.
        if (force && report.cached) quotaForcedAt.current = 0;
        setQuota(report); setQuotaKey(effectiveWorkspaceKey);
      })
      .catch((cause) => { if (!cancelled) setQuotaError(cause instanceof Error ? cause.message : '额度读取失败'); })
      .finally(() => { if (!cancelled) setQuotaLoading(false); });
    return () => { cancelled = true; };
  }, [visible, effectiveWorkspaceKey, quotaRevision]);

  useEffect(() => {
    if (loading) return;
    const saved = JSON.stringify({ conversationID, prompt, model, useWebSearch, target });
    try { sessionStorage.setItem(SESSION_KEY, saved); localStorage.setItem(SESSION_KEY, saved); } catch { /* Optional storage. */ }
  }, [conversationID, prompt, model, useWebSearch, target, loading]);

  useEffect(() => {
    if (!visible) setMobileOpen(false);
    const viewport = window.visualViewport;
    const resize = () => shellRef.current?.style.setProperty('--chat-height', `${viewport?.height || window.innerHeight}px`);
    resize();
    viewport?.addEventListener('resize', resize);
    return () => viewport?.removeEventListener('resize', resize);
  }, [visible]);

  useEffect(() => {
    const element = inputRef.current;
    if (element && visible) { element.style.height = '0px'; element.style.height = Math.min(Math.max(element.scrollHeight, 62), 180) + 'px'; }
  }, [prompt, visible]);

  useEffect(() => {
    const element = historyRef.current;
    if (element && visible && followBottom.current) { element.scrollTop = element.scrollHeight; setAtBottom(true); }
  }, [messages, visible]);

  useEffect(() => { activeConversationRef.current = conversationID; }, [conversationID]);

  useEffect(() => {
    if (!remoteRunning || !conversationID) return;
    let cancelled = false;
    const timer = setInterval(() => {
      void propsRef.current.onLoad(conversationID).then(({ item }) => {
        if (cancelled) return;
        setMessages(item.messages || []);
        setRemoteRunning(item.status === 'running' || item.status === 'queued');
      }).catch(() => { /* Keep history and retry on the next poll. */ });
    }, 2500);
    return () => { cancelled = true; clearInterval(timer); };
  }, [remoteRunning, conversationID]);

  function startNew() {
    if (abortRef.current) return;
    loadRevision.current++;
    setConversationID(''); setMessages([]); setOwner(''); setTitle('新对话'); setError(''); setPrompt(''); setFiles([]);
    setLoading(false); setLoadFailed(false); setRemoteRunning(false); setMobileOpen(false); setQuotaOpen(false);
    setRemoteOnly(false);
    cancelRename(); cancelEdit();
    if (!targets.some((item) => item.id === target)) setTarget('auto');
    followBottom.current = true;
    if (fileRef.current) fileRef.current.value = '';
    inputRef.current?.focus();
  }

  // Attachments arrive from the picker, a drop, or a paste, and all three go
  // through here so the size budget is enforced in one place. Rejections are
  // reported per file: silently dropping a screenshot is worse than saying no.
  function addFiles(incoming: File[]) {
    if (!incoming.length) return;
    if (running) { toast.info('正在生成，暂时不能添加附件'); return; }
    const accepted: File[] = [];
    const rejected: string[] = [];
    const seen = new Set(files.map((file) => `${file.name}\u0000${file.size}`));
    let total = files.reduce((sum, file) => sum + encodedAttachmentBytes(file), 0);
    const budget = MAX_REQUEST_BODY_BYTES - JSON_ENVELOPE_BYTES;
    for (const file of incoming) {
      if (!file.size) { rejected.push(`${file.name || '未命名文件'}（空文件）`); continue; }
      const key = `${file.name}\u0000${file.size}`;
      if (seen.has(key)) continue;
      const size = encodedAttachmentBytes(file);
      if (total + size > budget) {
        rejected.push(`${file.name}（${formatBytes(file.size)}，单次发送上限约 ${formatBytes(Math.round(budget * 3 / 4))}）`);
        continue;
      }
      seen.add(key);
      accepted.push(file);
      total += size;
    }
    if (accepted.length) setFiles((current) => [...current, ...accepted]);
    if (rejected.length) toast.error(`已跳过 ${rejected.length} 个文件：${rejected.join('；')}`);
  }

  function onDragEnter(event: React.DragEvent) {
    if (!event.dataTransfer?.types?.includes('Files')) return;
    event.preventDefault();
    dragDepth.current += 1;
    setDragging(true);
  }

  function onDragOver(event: React.DragEvent) {
    if (!event.dataTransfer?.types?.includes('Files')) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'copy';
  }

  function onDragLeave() {
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (!dragDepth.current) setDragging(false);
  }

  function onDrop(event: React.DragEvent) {
    if (!event.dataTransfer?.types?.includes('Files')) return;
    event.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    if (running) { toast.info('正在生成，暂时不能添加附件'); return; }
    addFiles(Array.from(event.dataTransfer.files || []));
  }

  const conversationLabel = (item: ConversationSummary) => item.title || item.request_prompt || item.preview || '新对话';

  function startRename(item: ConversationSummary) {
    // Remote-only transcripts have no row in this bridge's store, so a rename
    // would be accepted by the UI and then silently fail to persist.
    if (item.remote_only) { toast.info('该对话只存在于 Notion，无法在此重命名'); return; }
    setRenamingID(item.id); setRenameValue(conversationLabel(item));
  }

  function cancelRename() { setRenamingID(''); setRenameValue(''); }

  async function commitRename(id: string) {
    const next = renameValue.trim();
    const item = conversations.find((entry) => entry.id === id);
    if (!next || !item || next === conversationLabel(item)) { cancelRename(); return; }
    setBusyID(id);
    try {
      await AdminService.renameConversation(id, next);
      await propsRef.current.onRefreshConversations();
      if (id === conversationID) setTitle(next);
      cancelRename();
      toast.success('已重命名');
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : '重命名失败');
    } finally { setBusyID(''); }
  }

  async function removeConversation(item: ConversationSummary) {
    // Deleting removes the stored transcript, so the confirmation names the
    // conversation and says plainly that it cannot be undone.
    if (item.remote_only) { toast.info('该对话只存在于 Notion，无法在此删除'); return; }
    if (!window.confirm(`删除「${conversationLabel(item)}」？该对话的记录会一并移除，且不可恢复。`)) return;
    if (item.status === 'running' || item.status === 'queued') { toast.info('此对话正在生成，完成后再删除'); return; }
    setBusyID(item.id);
    try {
      await propsRef.current.onDeleteConversation(item.id);
      if (item.id === conversationID) startNew();
      toast.success('已删除');
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : '删除失败');
    } finally { setBusyID(''); }
  }

  // The open conversation is exported from what is on screen; any other one is
  // read first. Using the local snapshot keeps export offline and instant.
  async function exportConversation(id: string, label: string) {
    setBusyID(id);
    try {
      let items = id === conversationID ? messages : null;
      let exportTitle = label;
      let exportModel: string | undefined = id === conversationID ? model : undefined;
      let exportAccount: string | undefined = id === conversationID ? owner : undefined;
      if (!items) {
        const { item } = await propsRef.current.onLoad(id);
        items = item.messages || [];
        exportTitle = item.title || label;
        exportModel = item.model;
        exportAccount = item.account_email;
      }
      downloadTextFile(`${safeFileName(exportTitle)}.md`, conversationMarkdown(exportTitle, items, { model: exportModel, account: exportAccount }));
      toast.success('已导出 Markdown');
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : '导出失败');
    } finally { setBusyID(''); }
  }

  function startEdit(message: ConversationMessage) {
    if (!message.id) return;
    if (running) { toast.info('正在生成，暂时不能编辑'); return; }
    setEditingID(message.id); setEditValue(message.content || '');
  }

  function cancelEdit() { setEditingID(''); setEditValue(''); }

  async function commitEdit(message: ConversationMessage) {
    const next = editValue.trim();
    if (!conversationID || !message.id) return;
    if (next === (message.content || '').trim()) { cancelEdit(); return; }
    if (!next) { toast.error('内容不能为空'); return; }
    setSavingEdit(true);
    const targetID = conversationID;
    try {
      // The response carries the whole conversation, so the transcript is
      // replaced with the server's own view instead of a local guess.
      const { item } = await AdminService.editConversationMessage(targetID, message.id, next);
      if (!mounted.current) return;
      // Switching conversations while this save was in flight must not paste
      // one conversation's messages (and their message IDs) under another one.
      if (activeConversationRef.current !== targetID) {
        cancelEdit();
        await propsRef.current.onRefreshConversations();
        toast.success('已保存');
        return;
      }
      setMessages(item.messages || []);
      if (item.title) setTitle(item.title);
      cancelEdit();
      await propsRef.current.onRefreshConversations();
      toast.success('已保存');
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : '保存失败');
    } finally { setSavingEdit(false); }
  }

  async function performRun() {
    if (abortRef.current || loading || loadFailed || remoteRunning || (!prompt.trim() && !files.length)) return;
    if (!conversationID && target !== 'auto' && !selectedTarget) {
      setError('所选工作区已不可用，请重新选择商业工作区。');
      return;
    }
    const controller = new AbortController(); abortRef.current = controller;
    setRunning(true); setError(''); followBottom.current = true;
    const sentPrompt = prompt;
    const id = conversationID || 'conv_' + newID();
    const answerID = 'answer_' + newID();
    try {
      const attachments = await readFilesAsAttachments(files);
      if (controller.signal.aborted) return;
      if (!conversationID) setTitle(sentPrompt.slice(0, 60) || '附件对话');
      setConversationID(id);
      setMessages((current) => [...current,
        { id: 'user_' + answerID, role: 'user', content: sentPrompt, status: 'completed', attachments: files.map((file) => ({ name: file.name, content_type: file.type })) },
        { id: answerID, role: 'assistant', content: '', status: 'streaming' },
      ]);
      setPrompt(''); setFiles([]); if (fileRef.current) fileRef.current.value = '';
      const result = await onRun({ prompt: sentPrompt, model, use_web_search: useWebSearch, attachments, conversation_id: id,
        ...(selectedTarget ? { account_email: selectedTarget.email, workspace_id: selectedTarget.workspace } : {}),
      }, (delta) => {
        if (mounted.current) setMessages((current) => current.map((message) => message.id === answerID ? { ...message, content: (message.content || '') + delta } : message));
      }, controller.signal);
      if (!mounted.current) return;
      setConversationID(result.conversation_id || id);
      setMessages((current) => current.map((message) => message.id === answerID ? { ...message, content: result.text, status: 'completed', truncated: result.truncated } : message));
      // The turn just spent allowance; re-read so the indicator is not stale.
      refreshQuota();
      try {
        const { item } = await propsRef.current.onLoad(result.conversation_id || id);
        if (mounted.current) {
          setMessages(item.messages || []); setOwner(item.account_email || '');
          if (item.account_email && item.space_id) setTarget(targetID(item.account_email, item.space_id));
        }
      } catch { /* The completed answer remains visible. */ }
    } catch (cause) {
      if (!mounted.current) return;
      setError(controller.signal.aborted ? '已停止生成' : cause instanceof Error ? cause.message : '生成失败');
      setMessages((current) => current.map((item) => item.id === answerID ? { ...item, status: 'failed' } : item));
      if (!controller.signal.aborted) setPrompt((current) => current || sentPrompt);
    } finally { abortRef.current = null; if (mounted.current) setRunning(false); }
  }

  // Totals cover the conversation on screen. Process steps are excluded: their
  // text is upstream tool chatter, not something the turn actually billed for.
  const inputTokens = messages.filter((message) => message.role === 'user').reduce((sum, message) => sum + estimateTokens(message.content), 0);
  const outputTokens = messages.filter((message) => message.role === 'assistant').reduce((sum, message) => sum + estimateTokens(message.content), 0);

  const quotaWindows = [
    shortWindow ? { key: 'short', window: shortWindow, fallback: '短窗口' } : null,
    longWindow ? { key: 'long', window: longWindow, fallback: '长周期' } : null,
  ].filter((item): item is { key: string; window: AIUsageRateLimitWindow; fallback: string } => item !== null);

  const quotaPanel = !quotaOpen ? null : <>
    <div className="chat-quota-backdrop" onClick={() => setQuotaOpen(false)} />
    <div className="chat-quota-panel" role="dialog" aria-label="工作区额度">
      <div className="chat-quota-panel-head">
        <span>工作区额度</span>
        <button type="button" className="chat-icon" aria-label="刷新额度" disabled={quotaLoading || quotaRowsLoading} onClick={() => refreshQuota()}>
          {quotaLoading || quotaRowsLoading ? <LoaderCircle size={14} className="animate-spin" /> : <RefreshCw size={14} />}
        </button>
      </div>
      {quotaError ? <p className="chat-quota-empty">{quotaError}</p> : null}
      {!quotaError && !effectiveWorkspace ? (
        quotaRows === null ? <p className="chat-quota-empty">正在读取…</p>
          : !quotaRows.length ? <p className="chat-quota-empty">暂无可读取额度的商业工作区。</p>
          : <div className="chat-quota-list">{quotaRows.map((row) => {
            const percent = quotaRemainingPercent(row.usage?.rate_limit?.short ?? row.usage?.rate_limit?.long);
            return <div className="chat-quota-row" key={(row.email || '') + (row.space_id || '')}>
              <span className="chat-quota-row-name">{row.workspace_name || row.space_id}</span>
              <span className={'chat-quota-row-value is-' + (percent === null ? 'ok' : quotaTone(percent))}>{percent === null ? '未知' : `剩余 ${percent}%`}</span>
            </div>;
          })}</div>
      ) : null}
      {!quotaError && effectiveWorkspace ? <>
        <p className="chat-quota-workspace">{effectiveWorkspace.name} · {effectiveWorkspace.email}</p>
        {quotaLoading && !activeQuota ? <p className="chat-quota-empty">正在读取…</p> : null}
        {activeQuota && activeQuota.status !== 'ok' ? <p className="chat-quota-empty">{activeQuota.detail || '额度不可用'}</p> : null}
        {quotaWindows.length ? quotaWindows.map((item) => {
          const percent = quotaRemainingPercent(item.window);
          return <div className="chat-quota-window" key={item.key}>
            <div className="chat-quota-window-top">
              <strong>{quotaWindowLabel(item.window.label, item.fallback)}</strong>
              <span>{percent === null ? '未知' : `剩余 ${percent}%`}</span>
            </div>
            {percent === null ? null : <span className={'chat-quota-bar is-' + quotaTone(percent)}><b style={{ width: `${percent}%` }} /></span>}
            <div className="chat-quota-window-meta">{formatQuotaAmount(item.window)}</div>
          </div>;
        }) : (quotaLoading || !activeQuota ? null : <p className="chat-quota-empty">上游未返回窗口额度。</p>)}
        <p className="chat-quota-note">数值来自 Notion 上游，仅供估算，不代表实际扣费。</p>
      </> : null}
    </div>
  </>;

  const sidebar = <div className="chat-sidebar-content">
    <div className="chat-brand"><span className="chat-mark"><Sparkles size={17} /></span><span>Notion AI</span><button className="chat-icon ml-auto hidden lg:flex" aria-label="收起侧栏" onClick={() => setSidebarOpen(false)}><PanelLeftClose size={17} /></button></div>
    <button className="chat-new" disabled={running} onClick={startNew}><Plus size={17} />新对话<span aria-hidden="true" className="ml-auto text-xs text-muted-foreground">＋</span></button>
    <label className="chat-search"><Search size={15} /><input aria-label="搜索对话" placeholder="搜索对话" value={filter} onChange={(event) => setFilter(event.target.value)} /></label>
    <div className="chat-history-label">最近对话</div>
    <nav className="chat-conversations" aria-label="历史会话">
      {history.map((item) => {
        const label = conversationLabel(item);
        const busy = busyID === item.id;
        if (renamingID === item.id) return <form key={item.id} className="chat-conversation is-renaming"
          onSubmit={(event) => { event.preventDefault(); void commitRename(item.id); }}>
          <input autoFocus aria-label="对话标题" value={renameValue} maxLength={72} disabled={busy}
            onChange={(event) => setRenameValue(event.target.value)}
            onKeyDown={(event) => { if (event.key === 'Escape') cancelRename(); }} />
          <button type="submit" className="chat-icon" aria-label="保存标题" disabled={busy || !renameValue.trim()}>{busy ? <LoaderCircle size={13} className="animate-spin" /> : <Check size={13} />}</button>
          <button type="button" className="chat-icon" aria-label="取消重命名" disabled={busy} onClick={cancelRename}><X size={13} /></button>
        </form>;
        return <div className="chat-conversation" key={item.id} data-active={item.id === conversationID ? 'true' : undefined}>
          <button className="chat-conversation-open" title={label} aria-current={item.id === conversationID ? 'page' : undefined} disabled={running} onClick={() => void openConversation(item.id)}>
            <MessageSquare size={14} /><span>{label}</span>{item.status === 'running' ? <LoaderCircle size={12} className="animate-spin" /> : null}
          </button>
          <div className="chat-conversation-actions">
            {/* Rename and delete both write to the local store; a remote-only
                transcript has nothing local to write to, so those two controls
                are withheld instead of offered and then failing on save. */}
            {item.remote_only ? null : <button className="chat-icon" aria-label={`重命名 ${label}`} disabled={busy || running} onClick={() => startRename(item)}><Pencil size={13} /></button>}
            <button className="chat-icon" aria-label={`导出 ${label}`} disabled={busy} onClick={() => void exportConversation(item.id, label)}>{busy ? <LoaderCircle size={13} className="animate-spin" /> : <Download size={13} />}</button>
            {item.remote_only ? null : <button className="chat-icon" aria-label={`删除 ${label}`} disabled={busy || running} onClick={() => void removeConversation(item)}><Trash2 size={13} /></button>}
          </div>
        </div>;
      })}
      {!history.length ? <p className="px-3 py-2 text-xs leading-6 text-muted-foreground">{filter ? '没有匹配的对话' : '对话会保存在这里，随时继续。'}</p> : null}
    </nav>
    <div className="chat-sidebar-footer">
      <button onClick={() => onNavigate('accounts')}><KeyRound size={16} />账号与工作区</button>
      <button onClick={() => onNavigate('settings')}><Settings2 size={16} />设置</button>
      <button onClick={() => onNavigate('dashboard')}><span className="chat-status-dot" />管理控制台<span className="ml-auto text-xs opacity-60">↗</span></button>
    </div>
  </div>;

  return <div className="chat-shell" ref={shellRef}>
    {sidebarOpen ? <aside className="chat-sidebar hidden lg:block">{sidebar}</aside> : null}
    <Dialog open={mobileOpen && visible} onOpenChange={setMobileOpen}><DialogContent aria-describedby={undefined} className="chat-mobile-sidebar !left-0 !top-0 !h-dvh !w-[280px] !max-w-[90vw] !translate-x-0 !translate-y-0 !bg-[var(--chat-side)] rounded-none border-0 p-0"><DialogTitle className="sr-only">历史会话与导航</DialogTitle>{sidebar}</DialogContent></Dialog>
    <section className="chat-main">
      <header className="chat-header">
        <button className="chat-icon lg:hidden" aria-label="打开导航" onClick={() => setMobileOpen(true)}><PanelLeftOpen size={18} /></button>
        {!sidebarOpen ? <button className="chat-icon hidden lg:flex" aria-label="展开侧栏" onClick={() => setSidebarOpen(true)}><PanelLeftOpen size={18} /></button> : null}
        <div className="min-w-0"><div className="chat-breadcrumb"><span className="chat-workspace-name">{boundTarget?.name || 'Notion AI'}</span><span>/</span><h1>{title}</h1></div></div>
        <div className="ml-auto flex shrink-0 items-center gap-1">
          {running ? <span className="chat-generating"><span />生成中</span> : null}
          {targets.length ? <div className="chat-quota">
            <button type="button" className="chat-quota-chip" aria-expanded={quotaOpen} aria-haspopup="dialog" onClick={toggleQuota}
              title={quotaError || (effectiveWorkspace ? `${effectiveWorkspace.name} 的工作区额度` : '各工作区剩余额度')}>
              <Gauge size={15} />
              <span>{quotaError && !activeQuota ? '额度不可用' : primaryPercent === null ? '额度' : `剩余 ${primaryPercent}%`}</span>
              {primaryPercent === null ? null : <i className={'chat-quota-bar is-' + quotaTone(primaryPercent)}><b style={{ width: `${primaryPercent}%` }} /></i>}
            </button>
            {quotaPanel}
          </div> : null}
          {conversationID ? <button className="chat-icon" aria-label="导出 Markdown" title="导出为 Markdown" disabled={loading || busyID === conversationID} onClick={() => void exportConversation(conversationID, title)}>{busyID === conversationID ? <LoaderCircle size={17} className="animate-spin" /> : <Download size={17} />}</button> : null}
          <button className="chat-icon" aria-label="切换明暗主题" onClick={() => setTheme(resolvedTheme === 'dark' ? 'light' : 'dark')}>{resolvedTheme === 'dark' ? <Sun size={17} /> : <Moon size={17} />}</button>
          <button className="chat-icon lg:hidden" aria-label="新对话" disabled={running} onClick={startNew}><Plus size={18} /></button>
        </div>
      </header>
      <div className="chat-scroll" ref={historyRef} aria-label="聊天记录" aria-busy={loading || running} onScroll={() => {
        const element = historyRef.current;
        if (!element) return;
        const near = element.scrollHeight - element.scrollTop - element.clientHeight < 90;
        followBottom.current = near; setAtBottom(near);
      }}>
        {loading ? <LoaderCircle className="mx-auto mt-16 size-5 animate-spin text-muted-foreground" aria-label="加载中" /> : null}
        {!loading && !messages.length && !loadFailed ? <div className="chat-welcome">
          <div className="chat-welcome-symbol"><Sparkles size={28} strokeWidth={1.35} /></div>
          <p className="chat-eyebrow">你的 AI 工作空间</p><h2>今天想推进什么？</h2><p>从一个问题、一份文档，或一个想法开始。</p>
          <div className="chat-starters">{['梳理思路与行动计划', '分析文档中的关键信息', '一起打磨一段文字'].map((text) => <button key={text} onClick={() => { setPrompt(text); inputRef.current?.focus(); }}>{text}<ArrowUp size={14} /></button>)}</div>
        </div> : null}
        <div className="chat-transcript">{messages.map((message, index) => {
          // A process step is Notion's own trail (search, tool call, thinking).
          // It is rendered as a slim marker instead of a chat bubble so it
          // reads as context for the answer rather than as a turn of its own.
          if ((message.role || '').toLowerCase() === 'step') return <div className="chat-step" key={message.id || index}>
            <span className="chat-step-label">{stepLabel(message.step_type)}</span>
            {message.content ? <span className="chat-step-text">{message.content}</span> : null}
          </div>;
          const isUser = message.role === 'user';
          const editing = Boolean(message.id) && editingID === message.id;
          return <article key={message.id || index} className={isUser ? 'chat-message chat-user' : 'chat-message chat-assistant'}>
            {!isUser ? <div className="chat-assistant-label"><Sparkles size={15} />Notion AI</div> : null}
            {editing ? <div className="chat-message-edit">
              <textarea autoFocus aria-label="编辑消息内容" value={editValue} disabled={savingEdit} onChange={(event) => setEditValue(event.target.value)}
                onKeyDown={(event) => { if (event.key === 'Escape') cancelEdit(); }} />
              <div className="chat-message-edit-actions">
                <button className="chat-icon" aria-label="保存修改" disabled={savingEdit || !editValue.trim()} onClick={() => void commitEdit(message)}>{savingEdit ? <LoaderCircle size={14} className="animate-spin" /> : <Check size={14} />}</button>
                <button className="chat-icon" aria-label="取消修改" disabled={savingEdit} onClick={cancelEdit}><X size={14} /></button>
              </div>
            </div> : <div className="chat-message-content">
              {isUser
                ? (message.content ? <p className="whitespace-pre-wrap">{message.content}</p> : null)
                : <ReactMarkdown remarkPlugins={[remarkGfm]} components={{
                  ...safeMarkdownComponents,
                  table: ({ children }) => <div className="chat-table"><table>{children}</table></div>,
                }}>{message.content || ''}</ReactMarkdown>}
              {!message.content && message.status === 'streaming' ? <div className="chat-thinking"><span /><span /><span /><span className="sr-only">正在生成</span></div> : null}
              {message.attachments?.length ? <div className="chat-attachments">{message.attachments.map((file, i) => {
                const label = file.name || `附件 ${i + 1}`;
                const isImage = attachmentIsImage(file);
                const icon = isImage ? <ImageIcon size={14} /> : <FileText size={14} />;
                // Files Notion itself shared carry a URL; earlier this rendered
                // as plain text, so the hello.py-style card was visible but inert.
                if (!file.url) return <span key={i} title={label}>{icon}<span>{label}</span></span>;
                return <a key={i} className="chat-attachment-link" href={file.url} target="_blank" rel="noreferrer noopener" title={`打开 ${label}`}>
                  {isImage ? <img src={file.url} alt="" loading="lazy" referrerPolicy="no-referrer" onError={(event) => { event.currentTarget.style.display = 'none'; }} /> : null}
                  {icon}<span>{label}</span>
                </a>;
              })}</div> : null}
            </div>}
            <ModelEvidence message={message} models={modelCatalog} />
            <div className="chat-message-actions">
              {message.edited_at ? <span className="chat-edited">已编辑</span> : null}
              {message.status === 'failed' ? <span>未完成</span> : null}
              {message.truncated ? <span className="chat-truncated" title="上游在回答写完之前结束了流，这段回答可能不完整">已截断</span> : null}
              {message.content ? <span className="chat-token-hint" title="按字符数估算，非上游计数">≈ {formatTokens(estimateTokens(message.content))} tokens</span> : null}
              <button className="chat-icon" aria-label="编辑消息" disabled={running || !message.id || remoteOnly} onClick={() => startEdit(message)}><Pencil size={14} /></button>
              <button className="chat-icon" aria-label="复制消息" disabled={!message.content} onClick={() => void copyText(message.content || '').then(() => { setCopied(String(index)); setTimeout(() => setCopied(''), 1600); }).catch(() => toast.error('复制失败'))}>{copied === String(index) ? <Check size={14} /> : <Copy size={14} />}</button>
            </div>
          </article>;
        })}</div>
      </div>
      <div className="chat-composer-area">
        {!atBottom && messages.length ? <button className="chat-latest" onClick={() => {
          followBottom.current = true;
          if (historyRef.current) historyRef.current.scrollTop = historyRef.current.scrollHeight;
          setAtBottom(true);
        }}><ArrowDown size={15} />回到最新</button> : null}
        {error ? <div className="chat-error" role="status">{error}{loadFailed ? <button onClick={() => void openConversation(conversationID)}>重新加载</button> : null}</div> : null}
        {remoteRunning ? <div className="chat-remote"><LoaderCircle size={14} className="animate-spin" />此对话正在生成，完成后会自动更新。</div> : null}
        <form className={'chat-composer' + (dragging ? ' is-dragging' : '')}
          onSubmit={(event) => { event.preventDefault(); void performRun(); }}
          onDragEnter={onDragEnter} onDragOver={onDragOver} onDragLeave={onDragLeave} onDrop={onDrop}>
          {dragging ? <div className="chat-dropzone" aria-hidden="true"><Paperclip size={18} /><span>松开即可添加文件</span></div> : null}
          {files.length ? <div className="chat-attachments">{files.map((file, index) => <span key={index} title={`${file.name} · ${formatBytes(file.size)}`}>{attachmentIsImage({ content_type: file.type, name: file.name }) ? <ImageIcon size={14} /> : <FileText size={14} />}<span>{file.name}</span><em className="chat-attachment-size">{formatBytes(file.size)}</em><button type="button" aria-label={'移除 ' + file.name} disabled={running} onClick={() => setFiles((current) => current.filter((_, i) => i !== index))}><X size={13} /></button></span>)}</div> : null}
          <textarea ref={inputRef} aria-label="消息" placeholder="发送消息，或拖入 / 粘贴文件一起讨论…" value={prompt} onChange={(event) => setPrompt(event.target.value)} disabled={loading || loadFailed || remoteRunning} rows={2}
            onPaste={(event) => {
              // A pasted screenshot arrives as a file, not as text. Let text
              // paste through untouched so the composer still behaves normally.
              const pasted = Array.from(event.clipboardData?.files || []);
              if (!pasted.length) return;
              event.preventDefault();
              addFiles(pasted);
            }}
            onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void performRun(); } }} />
          <div className="chat-composer-toolbar">
            <input ref={fileRef} type="file" className="hidden" multiple disabled={running} onChange={(event) => { addFiles(Array.from(event.target.files || [])); event.target.value = ''; }} />
            <button type="button" className="chat-icon" aria-label="添加附件" disabled={loading || running} onClick={() => fileRef.current?.click()}><Paperclip size={18} /></button>
            <Select value={model} onValueChange={setModel} disabled={running || remoteRunning || availableModels.length === 1}><SelectTrigger className="chat-model-select" aria-label="模型"><SelectValue /></SelectTrigger><SelectContent>{availableModels.map((item) => <SelectItem key={item.id} value={item.id}>{item.name || item.id}</SelectItem>)}</SelectContent></Select>
            <button type="button" className={'chat-tool' + (useWebSearch ? ' is-active' : '')} aria-label="联网搜索" aria-pressed={useWebSearch} disabled={running} onClick={() => setUseWebSearch(!useWebSearch)}><Globe2 size={16} /><span>联网</span></button>
            {running ? <button type="button" className="chat-send" aria-label="停止" onClick={() => abortRef.current?.abort()}><Square size={15} fill="currentColor" /></button> : <button type="submit" className="chat-send" aria-label="发送" disabled={loading || loadFailed || remoteRunning || (!prompt.trim() && !files.length)}><ArrowUp size={19} /></button>}
          </div>
        </form>
        <div className="px-2 pt-1 text-xs text-muted-foreground" role="note">{autoOnly ? "此工作区仅支持 Auto，由 Notion 分配模型。" : availableModels.length === 1 ? "模型选择能力尚未确认或没有可选模型，可先使用 Auto；在账号页刷新模型能力。" : "手动选择模型时，仅使用支持该模型的工作区。"}</div>
        <div className="chat-composer-footer">
          <Select value={target} onValueChange={setTarget} disabled={Boolean(conversationID) || loading || running}><SelectTrigger className="chat-workspace-select" aria-label="商业工作区" title={owner || boundTarget?.email}><SelectValue placeholder={conversationID ? '会话绑定工作区' : '自动选择商业工作区'} /></SelectTrigger><SelectContent><SelectItem value="auto">{conversationID ? '会话绑定工作区' : '自动选择商业工作区'}</SelectItem>{targets.map((item) => <SelectItem value={item.id} key={item.id}>{item.name} · {item.tier} · {item.email}</SelectItem>)}{target !== 'auto' && !selectedTarget ? <SelectItem value={target} disabled>{boundTarget ? `${boundTarget.name} · ${boundTarget.tier}` : '原工作区'} · 当前不可用</SelectItem> : null}</SelectContent></Select>
          {messages.length ? <span className="chat-token-total" role="note" title="按字符数估算，非上游计数">≈ 输入 {formatTokens(inputTokens)} · 输出 {formatTokens(outputTokens)} tokens（估算）</span> : null}
          <span className="chat-keyboard-hint">Enter 发送<span> · </span>Shift + Enter 换行</span>
        </div>
      </div>
    </section>
  </div>;
}
