'use client';

import { useEffect, useRef, useState } from 'react';
import { ArrowDown, ArrowUp, Check, Copy, FileText, Gauge, Globe2, KeyRound, LoaderCircle, MessageSquare, Moon, PanelLeftClose, PanelLeftOpen, Paperclip, Plus, RefreshCw, Search, Settings2, Sparkles, Square, Sun, X } from 'lucide-react';
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

export function ChatWorkspace({ models, defaultModel, defaultWebSearch, initialConversationID, onResumeHandled, onLoad, onRun, conversations, accounts, onNavigate, visible }: {
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
  visible: boolean;
}) {
  const [prompt, setPrompt] = useState('');
  const [model, setModel] = useState(defaultModel || models[0]?.id || 'auto');
  const [useWebSearch, setUseWebSearch] = useState(defaultWebSearch);
  const [conversationID, setConversationID] = useState('');
  const [messages, setMessages] = useState<ConversationMessage[]>([]);
  const [files, setFiles] = useState<File[]>([]);
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
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const historyRef = useRef<HTMLDivElement>(null);
  const shellRef = useRef<HTMLDivElement>(null);
  const mounted = useRef(false);
  const loadRevision = useRef(0);
  const followBottom = useRef(true);
  const propsRef = useRef({ onLoad, onResumeHandled });
  propsRef.current = { onLoad, onResumeHandled };
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
    try {
      const { item } = await propsRef.current.onLoad(id);
      if (!mounted.current || revision !== loadRevision.current) return;
      setMessages(item.messages || []); setOwner(item.account_email || '');
      setTitle(item.title || item.request_prompt?.slice(0, 60) || '对话');
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
    if (!targets.some((item) => item.id === target)) setTarget('auto');
    followBottom.current = true;
    if (fileRef.current) fileRef.current.value = '';
    inputRef.current?.focus();
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
      setMessages((current) => current.map((message) => message.id === answerID ? { ...message, content: result.text, status: 'completed' } : message));
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
      {history.map((item) => <button key={item.id} title={item.title || item.request_prompt || item.preview || '新对话'} aria-current={item.id === conversationID ? 'page' : undefined} disabled={running} onClick={() => void openConversation(item.id)}>
        <MessageSquare size={14} /><span>{item.title || item.request_prompt || item.preview || '新对话'}</span>{item.status === 'running' ? <LoaderCircle size={12} className="animate-spin" /> : null}
      </button>)}
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
        <div className="chat-transcript">{messages.map((message, index) => <article key={message.id || index} className={message.role === 'user' ? 'chat-message chat-user' : 'chat-message chat-assistant'}>
          {message.role !== 'user' ? <div className="chat-assistant-label"><Sparkles size={15} />Notion AI</div> : null}
          <div className="chat-message-content">{message.role === 'user' ? <p className="whitespace-pre-wrap">{message.content}</p> : <ReactMarkdown remarkPlugins={[remarkGfm]} components={{
            ...safeMarkdownComponents,
            table: ({ children }) => <div className="chat-table"><table>{children}</table></div>,
          }}>{message.content || ''}</ReactMarkdown>}
          {!message.content && message.status === 'streaming' ? <div className="chat-thinking"><span /><span /><span /><span className="sr-only">正在生成</span></div> : null}
          {message.attachments?.length ? <div className="chat-attachments">{message.attachments.map((file, i) => <span key={i}><FileText size={14} />{file.name}</span>)}</div> : null}
          </div>
          <ModelEvidence message={message} models={modelCatalog} />
          <div className="chat-message-actions">{message.status === 'failed' ? <span>未完成</span> : null}<button className="chat-icon" aria-label="复制消息" disabled={!message.content} onClick={() => void copyText(message.content || '').then(() => { setCopied(String(index)); setTimeout(() => setCopied(''), 1600); }).catch(() => toast.error('复制失败'))}>{copied === String(index) ? <Check size={14} /> : <Copy size={14} />}</button></div>
        </article>)}</div>
      </div>
      <div className="chat-composer-area">
        {!atBottom && messages.length ? <button className="chat-latest" onClick={() => {
          followBottom.current = true;
          if (historyRef.current) historyRef.current.scrollTop = historyRef.current.scrollHeight;
          setAtBottom(true);
        }}><ArrowDown size={15} />回到最新</button> : null}
        {error ? <div className="chat-error" role="status">{error}{loadFailed ? <button onClick={() => void openConversation(conversationID)}>重新加载</button> : null}</div> : null}
        {remoteRunning ? <div className="chat-remote"><LoaderCircle size={14} className="animate-spin" />此对话正在生成，完成后会自动更新。</div> : null}
        <form className="chat-composer" onSubmit={(event) => { event.preventDefault(); void performRun(); }}>
          {files.length ? <div className="chat-attachments">{files.map((file, index) => <span key={index}><FileText size={14} /><span>{file.name}</span><button type="button" aria-label={'移除 ' + file.name} disabled={running} onClick={() => setFiles((current) => current.filter((_, i) => i !== index))}><X size={13} /></button></span>)}</div> : null}
          <textarea ref={inputRef} aria-label="消息" placeholder="发送消息，或添加文件一起讨论…" value={prompt} onChange={(event) => setPrompt(event.target.value)} disabled={loading || loadFailed || remoteRunning} rows={2}
            onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void performRun(); } }} />
          <div className="chat-composer-toolbar">
            <input ref={fileRef} type="file" className="hidden" multiple disabled={running} onChange={(event) => setFiles((current) => [...current, ...Array.from(event.target.files || [])])} />
            <button type="button" className="chat-icon" aria-label="添加附件" disabled={loading || running} onClick={() => fileRef.current?.click()}><Paperclip size={18} /></button>
            <Select value={model} onValueChange={setModel} disabled={running || remoteRunning || availableModels.length === 1}><SelectTrigger className="chat-model-select" aria-label="模型"><SelectValue /></SelectTrigger><SelectContent>{availableModels.map((item) => <SelectItem key={item.id} value={item.id}>{item.name || item.id}</SelectItem>)}</SelectContent></Select>
            <button type="button" className={'chat-tool' + (useWebSearch ? ' is-active' : '')} aria-label="联网搜索" aria-pressed={useWebSearch} disabled={running} onClick={() => setUseWebSearch(!useWebSearch)}><Globe2 size={16} /><span>联网</span></button>
            {running ? <button type="button" className="chat-send" aria-label="停止" onClick={() => abortRef.current?.abort()}><Square size={15} fill="currentColor" /></button> : <button type="submit" className="chat-send" aria-label="发送" disabled={loading || loadFailed || remoteRunning || (!prompt.trim() && !files.length)}><ArrowUp size={19} /></button>}
          </div>
        </form>
        <div className="px-2 pt-1 text-xs text-muted-foreground" role="note">{autoOnly ? "此工作区仅支持 Auto，由 Notion 分配模型。" : availableModels.length === 1 ? "模型选择能力尚未确认或没有可选模型，可先使用 Auto；在账号页刷新模型能力。" : "手动选择模型时，仅使用支持该模型的工作区。"}</div>
        <div className="chat-composer-footer">
          <Select value={target} onValueChange={setTarget} disabled={Boolean(conversationID) || loading || running}><SelectTrigger className="chat-workspace-select" aria-label="商业工作区" title={owner || boundTarget?.email}><SelectValue placeholder={conversationID ? '会话绑定工作区' : '自动选择商业工作区'} /></SelectTrigger><SelectContent><SelectItem value="auto">{conversationID ? '会话绑定工作区' : '自动选择商业工作区'}</SelectItem>{targets.map((item) => <SelectItem value={item.id} key={item.id}>{item.name} · {item.tier} · {item.email}</SelectItem>)}{target !== 'auto' && !selectedTarget ? <SelectItem value={target} disabled>{boundTarget ? `${boundTarget.name} · ${boundTarget.tier}` : '原工作区'} · 当前不可用</SelectItem> : null}</SelectContent></Select>
          <span className="chat-keyboard-hint">Enter 发送<span> · </span>Shift + Enter 换行</span>
        </div>
      </div>
    </section>
  </div>;
}
