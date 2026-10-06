'use client';
import { useEffect, useRef, useState } from 'react';
import { Dialog, DialogContent, DialogTitle } from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { AdminService } from '@/lib/services/admin/admin.service';

type Workspace = { id: string; email: string; workspace: string; name: string };
type Note = { id: string; title: string; text: string; partial: boolean };
export function NotebookDialog({ open, onOpenChange, targets, current, referenceDisabled, onReference }: {
  open: boolean; onOpenChange: (open: boolean) => void; targets: Workspace[];
  current?: Workspace | null; referenceDisabled: boolean; onReference: (note: Note, workspace: Workspace) => void;
}) {
  const [target, setTarget] = useState('');
  const [items, setItems] = useState<Array<{ id: string; title: string }> | null>(null);
  const [note, setNote] = useState<Note | null>(null);
  const [filter, setFilter] = useState('');
  const [source, setSource] = useState<'shared' | 'private'>('shared');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const revision = useRef(0);
  const workspace = targets.find((item) => item.id === target);
  useEffect(() => {
    revision.current++;
    setTarget(targets.some((item) => item.id === current?.id) ? current!.id : targets[0]?.id || '');
    setItems(null); setNote(null); setError(''); setBusy(false); setFilter('');
  // Reset on opening or a change of the conversation's workspace, not on each parent render.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, current?.id]);
  async function read(pageID?: string) {
    if (!workspace || busy) return;
    const request = ++revision.current;
    setBusy(true); setError('');
    if (pageID) setNote(null);
    try {
      if (pageID) {
        const result = await AdminService.getNote(workspace.email, workspace.workspace, pageID);
        if (request === revision.current) setNote(result.item);
      } else {
        const result = await AdminService.getNotes(workspace.email, workspace.workspace, items !== null, source);
        if (request === revision.current) setItems(result.items);
      }
    } catch (cause) {
      if (request === revision.current) setError(cause instanceof Error ? cause.message : '笔记读取失败');
    } finally { if (request === revision.current) setBusy(false); }
  }
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent aria-describedby={undefined} className="!max-w-3xl max-h-[88dvh] overflow-y-auto">
    <DialogTitle>工作区笔记</DialogTitle>
    <p className="text-xs leading-6 text-muted-foreground">浏览所选工作区的共享或私人页面，将内容快照引用到输入框。引用不会修改笔记，也不会自动发送消息。</p>
    <Select value={target} disabled={busy} onValueChange={(value) => { revision.current++; setTarget(value); setItems(null); setNote(null); setError(''); setFilter(''); }}>
      <SelectTrigger aria-label="笔记工作区"><SelectValue placeholder="选择工作区" /></SelectTrigger>
      <SelectContent>{targets.map((item) => <SelectItem value={item.id} key={item.id}>{item.name} · {item.email}</SelectItem>)}</SelectContent>
    </Select>
    <Select value={source} disabled={busy} onValueChange={(value: 'shared' | 'private') => { revision.current++; setSource(value); setItems(null); setNote(null); setError(''); setFilter(''); }}>
      <SelectTrigger aria-label="笔记范围"><SelectValue /></SelectTrigger>
      <SelectContent><SelectItem value="shared">共享页面</SelectItem><SelectItem value="private">私人页面</SelectItem></SelectContent>
    </Select>
    <div className="flex flex-wrap items-center gap-2">
      <Button disabled={busy || !workspace} variant="outline" onClick={() => void read()}>{busy ? '读取中…' : items === null ? '读取笔记' : '刷新笔记列表'}</Button>
      {items ? <Input aria-label="搜索笔记" className="flex-1 min-w-32" placeholder="搜索已读取的笔记" value={filter} onChange={(event) => setFilter(event.target.value)} /> : null}
    </div>
    {error ? <p role="alert" className="text-xs text-destructive">{error}</p> : null}
    <div className="grid gap-4 md:grid-cols-[200px_minmax(0,1fr)]">
      <div className="max-h-80 overflow-y-auto space-y-1">
        {items?.filter((item) => item.title.toLocaleLowerCase().includes(filter.toLocaleLowerCase())).map((item) => <button key={item.id} disabled={busy}
          className="w-full rounded-lg p-3 text-left text-sm hover:bg-muted aria-[current=true]:bg-accent" aria-current={note?.id === item.id}
          onClick={() => void read(item.id)}>{item.title}</button>)}
        {items?.length === 0 ? <p className="text-xs text-muted-foreground">此范围没有返回页面，可切换共享／私人页面。这不是整个工作区的完整搜索。</p> : null}
      </div>
      <div className="min-w-0">
        {note ? <div className="space-y-3">
          <h3 className="font-medium">{note.title}</h3>
          {note.partial ? <p className="text-xs text-muted-foreground">这是部分文本预览，复杂内容、子页面或后续分页请在 Notion 查看。</p> : null}
          <pre className="max-h-80 overflow-y-auto whitespace-pre-wrap break-words rounded-lg bg-muted p-4 text-xs leading-6 font-sans">{note.text || '此页面没有可预览的文本。'}</pre>
          <div className="flex flex-wrap gap-3 items-center">
            <Button disabled={busy || referenceDisabled || !note.text || !workspace} onClick={() => { if (workspace) { onReference(note, workspace); onOpenChange(false); } }}>引用到输入框</Button>
            <a className="text-xs text-primary underline" href={'https://www.notion.so/' + encodeURIComponent(note.id.replaceAll('-', ''))} target="_blank" rel="noreferrer noopener">在 Notion 中编辑</a>
          </div>
        </div> : <p className="text-sm text-muted-foreground p-4">选择一篇笔记以预览内容。</p>}
      </div>
    </div>
  </DialogContent></Dialog>;
}
