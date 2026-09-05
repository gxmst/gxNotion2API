'use client';

import { useEffect, useRef, useState } from 'react';
import { Bot, Copy, FileImage, LoaderCircle, Paperclip, Plus, RefreshCcw, SendHorizontal, Square, User, X } from 'lucide-react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { Textarea } from '@/components/ui/textarea';
import { copyText, readFilesAsAttachments } from '@/lib/services/core/api-client';
import type { ChatRunInput, ChatRunResult, ConversationDetailPayload, ConversationMessage, ModelItem } from '@/lib/services/admin/types';

const SESSION_KEY = 'notion2api-chat-session';

function randomID() {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID().replace(/-/g, '');
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) => byte.toString(16).padStart(2, '0')).join('');
}

export function TesterPanel({
  models, defaultModel, defaultWebSearch, initialConversationID, onResumeHandled, onLoad, onRun,
}: {
  models: ModelItem[];
  defaultModel?: string;
  defaultWebSearch: boolean;
  initialConversationID?: string;
  onResumeHandled: () => void;
  onLoad: (id: string) => Promise<ConversationDetailPayload>;
  onRun: (payload: ChatRunInput, onDelta: (text: string) => void, signal: AbortSignal) => Promise<ChatRunResult>;
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
  const [owner, setOwner] = useState('');
  const abortRef = useRef<AbortController | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const historyRef = useRef<HTMLDivElement>(null);
  const mounted = useRef(false);
  const initialRef = useRef(initialConversationID);

  useEffect(() => {
    mounted.current = true;
    let cancelled = false;
    let saved: { conversationID?: string; prompt?: string; model?: string; useWebSearch?: boolean } = {};
    try {
      const value: unknown = JSON.parse(sessionStorage.getItem(SESSION_KEY) || '{}');
      if (value && typeof value === 'object' && !Array.isArray(value)) saved = value;
    } catch { /* Storage is optional. */ }
    const resume = initialRef.current;
    const id = resume || (typeof saved.conversationID === 'string' ? saved.conversationID : '');
    if (!resume) setPrompt(typeof saved.prompt === 'string' ? saved.prompt : '');
    if (saved.model && models.some((item) => item.id === saved.model)) setModel(saved.model);
    if (typeof saved.useWebSearch === 'boolean') setUseWebSearch(saved.useWebSearch);
    setConversationID(id);
    if (resume) onResumeHandled();
    if (id) {
      void onLoad(id).then(({ item }) => {
        if (cancelled) return;
        setMessages(item.messages || []);
        setOwner(item.account_email || '');
        setRemoteRunning(item.status === 'running');
        if (resume && item.model && models.some((model) => model.id === item.model)) setModel(item.model);
        if (item.status === 'running') setError('这个会话仍在生成，请稍后重新打开。');
      }).catch((cause) => {
        if (!cancelled) { setLoadFailed(true); setError(cause instanceof Error ? cause.message : '会话加载失败'); }
      }).finally(() => { if (!cancelled) setLoading(false); });
    } else { setLoading(false); }
    return () => { cancelled = true; mounted.current = false; abortRef.current?.abort(); };
    // The selected conversation is consumed once when this panel opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (loading) return;
    try { sessionStorage.setItem(SESSION_KEY, JSON.stringify({ conversationID, prompt, model, useWebSearch })); } catch { /* Storage is optional. */ }
  }, [conversationID, prompt, model, useWebSearch, loading]);

  useEffect(() => {
    const history = historyRef.current;
    if (history) history.scrollTop = history.scrollHeight;
  }, [messages]);

  async function reloadConversation() {
    setLoading(true);
    try {
      const { item } = await onLoad(conversationID);
      if (!mounted.current) return;
      setMessages(item.messages || []); setOwner(item.account_email || ''); setLoadFailed(false);
      setRemoteRunning(item.status === 'running');
      setError(item.status === 'running' ? '这个会话仍在生成，请稍后刷新。' : '');
    } catch (cause) {
      if (mounted.current) { setLoadFailed(true); setError(cause instanceof Error ? cause.message : '会话加载失败'); }
    } finally { if (mounted.current) setLoading(false); }
  }

  async function performRun() {
    if (abortRef.current || loading || loadFailed || remoteRunning || (!prompt.trim() && !files.length)) return;
    const controller = new AbortController();
    abortRef.current = controller;
    setRunning(true);
    setError('');
    const sentPrompt = prompt;
    const id = conversationID || `conv_${randomID()}`;
    const answerID = `answer_${randomID()}`;
    try {
      const attachments = await readFilesAsAttachments(files);
      if (controller.signal.aborted) return;
      setConversationID(id);
      setMessages((current) => [...current,
        { id: `user_${answerID}`, role: 'user', content: sentPrompt, status: 'completed', attachments: files.map((file) => ({ name: file.name, content_type: file.type })) },
        { id: answerID, role: 'assistant', content: '', status: 'streaming' },
      ]);
      setPrompt('');
      setFiles([]);
      if (fileRef.current) fileRef.current.value = '';
      const result = await onRun({ prompt: sentPrompt, model, use_web_search: useWebSearch, attachments, conversation_id: id }, (delta) => {
        if (mounted.current) setMessages((current) => current.map((message) => message.id === answerID ? { ...message, content: (message.content || '') + delta } : message));
      }, controller.signal);
      if (!mounted.current) return;
      setConversationID(result.conversation_id || id);
      setMessages((current) => current.map((message) => message.id === answerID ? { ...message, content: result.text, status: 'completed' } : message));
      try {
        const { item } = await onLoad(result.conversation_id || id);
        if (mounted.current) { setMessages(item.messages || []); setOwner(item.account_email || ''); }
      } catch { /* The completed response remains visible when history refresh fails. */ }
    } catch (cause) {
      if (!mounted.current) return;
      const message = controller.signal.aborted ? '已停止生成' : cause instanceof Error ? cause.message : '生成失败';
      setError(message);
      setMessages((current) => current.map((item) => item.id === answerID ? { ...item, status: 'failed' } : item));
      if (!controller.signal.aborted) setPrompt((current) => current || sentPrompt);
    } finally {
      abortRef.current = null;
      if (mounted.current) setRunning(false);
    }
  }

  function startNew() {
    setConversationID(''); setMessages([]); setOwner(''); setError(''); setPrompt(''); setFiles([]);
    setLoadFailed(false); setRemoteRunning(false);
    if (fileRef.current) fileRef.current.value = '';
  }

  return (
    <section className="mx-auto flex min-h-[600px] w-full max-w-5xl flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b pb-4">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold">Notion AI</h1>
          {owner ? <p className="mt-1 break-all text-xs text-muted-foreground">{owner}</p> : null}
        </div>
        <Button variant="outline" onClick={startNew} disabled={running || loading}><Plus className="size-4" />新对话</Button>
      </div>

      <div className="flex flex-wrap items-center gap-4">
        <div className="w-full sm:w-64">
          <Select value={model} onValueChange={setModel} disabled={running}>
            <SelectTrigger className="w-full" aria-label="模型"><SelectValue /></SelectTrigger>
            <SelectContent>{models.map((item) => <SelectItem key={item.id} value={item.id}>{item.name || item.id}</SelectItem>)}</SelectContent>
          </Select>
        </div>
        <label className="flex items-center gap-2 text-sm"><Switch checked={useWebSearch} onCheckedChange={setUseWebSearch} disabled={running} aria-label="联网搜索" />联网搜索</label>
      </div>

      <div ref={historyRef} className="h-[min(58vh,640px)] min-h-64 overflow-y-auto overscroll-contain px-1" aria-label="聊天记录" aria-busy={loading || running}>
        {loading ? <LoaderCircle className="mx-auto mt-10 size-5 animate-spin" aria-label="加载中" /> : null}
        {!loading && !messages.length ? <p className="py-16 text-center text-sm text-muted-foreground">新对话</p> : null}
        {messages.map((message, index) => (
          <article key={message.id || index} className="flex min-w-0 gap-3 border-b border-border/50 py-5 last:border-0">
            <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-muted" aria-label={message.role === 'user' ? '你' : 'Notion AI'}>
              {message.role === 'user' ? <User className="size-4" /> : <Bot className="size-4" />}
            </span>
            <div className="min-w-0 flex-1">
              <div className="mb-2 flex items-center justify-between gap-2">
                <span className="text-xs font-semibold text-muted-foreground">{message.role === 'user' ? '你' : 'Notion AI'}</span>
                <Button size="icon" variant="ghost" className="size-7 shrink-0" title="复制消息" aria-label="复制消息" disabled={!message.content}
                  onClick={() => void copyText(message.content || '').then(() => toast.success('已复制')).catch(() => toast.error('复制失败'))}><Copy className="size-3.5" /></Button>
              </div>
              <div className="space-y-3 break-words text-sm leading-7 [overflow-wrap:anywhere] [&_a]:text-primary [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:pl-3 [&_h1]:text-lg [&_h2]:text-base [&_h3]:font-semibold [&_li]:ml-5 [&_ol]:list-decimal [&_ul]:list-disc [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:bg-muted [&_pre]:p-3 [&_code]:font-mono [&_code]:text-xs [&_img]:max-w-full">
                {message.role === 'user' ? <p className="whitespace-pre-wrap">{message.content || ''}</p> : <ReactMarkdown remarkPlugins={[remarkGfm]} components={{
                  a: ({ children, ...props }) => <a {...props} target="_blank" rel="noreferrer">{children}</a>,
                  table: ({ children }) => <div className="overflow-x-auto"><table className="w-full border-collapse [&_td]:border [&_td]:p-2 [&_th]:border [&_th]:p-2">{children}</table></div>,
                }}>{message.content || ''}</ReactMarkdown>}
                {!message.content && message.status === 'streaming' ? <LoaderCircle className="size-4 animate-spin" aria-label="正在生成" /> : null}
                {message.status === 'failed' ? <p className="text-xs text-muted-foreground">未完成</p> : null}
              </div>
              {message.attachments?.length ? <div className="mt-3 flex flex-wrap gap-2">{message.attachments.map((file, i) => <span key={i} className="flex max-w-full items-center gap-1 text-xs text-muted-foreground"><FileImage className="size-3 shrink-0" /><span className="break-all">{file.name}</span></span>)}</div> : null}
            </div>
          </article>
        ))}
      </div>

      <form className="space-y-3 border-t pt-4" onSubmit={(event) => { event.preventDefault(); void performRun(); }}>
        {error ? <p role="status" className="break-words text-sm text-destructive">{error}</p> : null}
        {loadFailed || remoteRunning ? <Button type="button" variant="outline" disabled={loading} onClick={() => void reloadConversation()}><RefreshCcw className="size-4" />刷新会话</Button> : null}
        {files.length ? <div className="flex flex-wrap gap-2">{files.map((file, index) => <div key={`${file.name}-${index}`} className="flex max-w-full items-center gap-1 text-xs"><Paperclip className="size-3 shrink-0" /><span className="break-all">{file.name}</span><Button type="button" size="icon" variant="ghost" className="size-6 shrink-0" disabled={running} title="移除附件" aria-label={`移除 ${file.name}`} onClick={() => setFiles((current) => current.filter((_, i) => i !== index))}><X className="size-3" /></Button></div>)}</div> : null}
        <Textarea aria-label="消息" placeholder="发送消息" value={prompt} onChange={(event) => setPrompt(event.target.value)} className="min-h-24 resize-y rounded-md" disabled={loading || loadFailed || remoteRunning}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void performRun(); }
          }} />
        <div className="flex items-center justify-between gap-3">
          <input ref={fileRef} type="file" className="hidden" multiple disabled={running} onChange={(event) => setFiles((current) => [...current, ...Array.from(event.target.files || [])])} />
          <Button type="button" variant="ghost" size="icon" title="添加附件" aria-label="添加附件" disabled={loading || running || loadFailed || remoteRunning} onClick={() => fileRef.current?.click()}><Paperclip className="size-4" /></Button>
          {running ? <Button type="button" variant="outline" onClick={() => abortRef.current?.abort()}><Square className="size-4" />停止</Button>
            : <Button type="submit" disabled={loading || loadFailed || remoteRunning || (!prompt.trim() && !files.length)}><SendHorizontal className="size-4" />发送</Button>}
        </div>
      </form>
    </section>
  );
}
