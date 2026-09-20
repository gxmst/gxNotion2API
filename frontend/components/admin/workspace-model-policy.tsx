'use client';

import { useEffect, useRef, useState } from 'react';
import { Button } from '@/components/ui/button';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { AdminService } from '@/lib/services/admin/admin.service';
import { ApiError } from '@/lib/services/core/api-client';
import type { ModelPolicySnapshot } from '@/lib/services/admin/types';

export function WorkspaceModelPolicy({ email, workspaceID, workspaceName, coolingDown }: {
  email: string; workspaceID: string; workspaceName: string; coolingDown?: boolean;
}) {
  const [scope, setScope] = useState<'personal' | 'custom'>('personal');
  const [snapshot, setSnapshot] = useState<ModelPolicySnapshot | null>(null);
  const [backup, setBackup] = useState<ModelPolicySnapshot | null>(null);
  const [modelID, setModelID] = useState('');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const alive = useRef(true);
  const inFlight = useRef(false);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);

  async function read() {
    if (inFlight.current || coolingDown) return;
    inFlight.current = true;
    setBusy(true); setMessage('');
    try {
      const value = await AdminService.getModelPolicy(email, workspaceID, scope);
      if (!alive.current) return;
      setSnapshot(value);
      const allowed = value.models.filter((model) => model.allowed);
      setModelID(allowed.length === 1 ? allowed[0].id : '');
    } catch (error) {
      if (alive.current) { setSnapshot(null); setMessage(error instanceof Error ? error.message : '读取失败'); }
    } finally { inFlight.current = false; if (alive.current) setBusy(false); }
  }

  async function apply(restore = false) {
    if (inFlight.current || coolingDown || !snapshot?.can_edit || (restore ? !backup : !modelID)) return;
    inFlight.current = true;
    setBusy(true); setMessage('');
    const before = snapshot;
    try {
      const value = await AdminService.updateModelPolicy({ email, workspace_id: workspaceID, scope, revision: snapshot.revision,
        action: restore ? 'restore' : 'lock',
        ...(restore && backup ? { restore_policy: backup.policy, restore_present: backup.policy_present } : { model_id: modelID }),
      });
      if (!alive.current) return;
      if (!restore && !backup && value.revision !== before.revision) setBackup(before);
      setSnapshot(value);
      const allowed = value.models.filter((model) => model.allowed);
      setModelID(allowed.length === 1 ? allowed[0].id : '');
      if (restore) setBackup(null);
      setMessage(restore ? '已恢复修改前的设置。' : '已确认工作区设置保存成功。聊天继续使用 Auto，实际模型以回答中的上游报告为准。');
    } catch (error) {
      if (alive.current) {
        // A rejected edit did not change the workspace. Only keep a new
        // backup when the write may have succeeded before losing its reply.
        const rejected = error instanceof ApiError && [400, 401, 403, 404, 409, 429].includes(error.status);
        if (!restore && !backup && !rejected) setBackup(before);
        setSnapshot(null); setMessage(`${error instanceof Error ? error.message : '保存失败'} 请先重新读取设置。`);
      }
    } finally { inFlight.current = false; if (alive.current) setBusy(false); }
  }

  const selected = snapshot?.models.find((model) => model.id === modelID);
  const allowed = snapshot?.models.filter((model) => model.allowed) || [];
  const editable = snapshot?.can_edit && !coolingDown;
  return <section aria-label="工作区模型设置" className="col-span-full space-y-3 rounded-lg border p-3 text-sm">
    <div><h3 className="font-medium">工作区模型设置</h3><p className="mt-1 break-words text-xs text-muted-foreground">{workspaceName} · {email}</p></div>
    <p className="text-xs leading-5 text-muted-foreground">仅在点击应用或恢复时修改 Notion 设置。修改影响整个工作区中此类代理的所有会话和成员；只有工作区所有者可操作。</p>
    <Select value={scope} disabled={busy} onValueChange={(value: 'personal' | 'custom') => {
      if (inFlight.current) return;
      setScope(value); setSnapshot(null); setBackup(null); setModelID(''); setMessage('');
    }}><SelectTrigger aria-label="代理类型"><SelectValue /></SelectTrigger><SelectContent>
      <SelectItem value="personal">普通 Notion 代理</SelectItem><SelectItem value="custom">自定义代理</SelectItem>
    </SelectContent></Select>
    <Button variant="outline" disabled={busy || coolingDown} onClick={() => void read()}>{busy ? '正在处理…' : '读取模型设置'}</Button>
    {coolingDown ? <p className="text-xs text-muted-foreground">账号冷却期间暂停读取和修改。</p> : null}
    {snapshot ? <>
      <p className="text-xs">当前权限：{snapshot.membership_type === 'owner' ? '工作区所有者' : snapshot.membership_type === 'unknown' ? '未确认' : snapshot.membership_type}。{!snapshot.can_edit ? '当前仅可查看，无法修改。' : ''}</p>
      <p className="text-xs leading-5 text-muted-foreground">当前目录内允许：{allowed.length ? allowed.map((model) => model.name).join('、') : '没有可用模型'}。{!snapshot.policy_present ? '尚未设置额外模型限制。' : ''}</p>
      <Select value={modelID} onValueChange={setModelID} disabled={busy || !editable}>
        <SelectTrigger aria-label="工作区目标模型"><SelectValue placeholder="选择希望 Auto 使用的模型" /></SelectTrigger>
        <SelectContent>{snapshot.models.map((model) => <SelectItem key={model.id} value={model.id} disabled={!model.available}>
          {model.name}{!model.available ? `（不可用${model.disabled_reason ? `：${model.disabled_reason}` : ''}）` : ''}
        </SelectItem>)}</SelectContent>
      </Select>
      <p className="text-xs leading-5 text-muted-foreground">应用后将允许目标模型，并排除当前目录内的其他模型与供应商。目录新增模型后需重新确认；此设置不会解除试用或套餐限制。</p>
      {selected ? <p className="text-xs">将应用到「{workspaceName}」的{scope === 'personal' ? '普通 Notion 代理' : '自定义代理'}：{selected.name}</p> : null}
      <div className="flex flex-wrap gap-2">
        <Button disabled={busy || !editable || !selected?.available} onClick={() => void apply()}>应用到整个工作区</Button>
        <Button variant="outline" disabled={busy || !editable || !backup} onClick={() => void apply(true)}>恢复本次修改前设置</Button>
      </div>
    </> : null}
    {backup && !snapshot ? <p className="text-xs text-muted-foreground">本页保留了修改前设置，重新读取后可手动恢复。</p> : null}
    {backup ? <p className="text-xs text-muted-foreground">恢复将使用本页首次修改前的设置；离开面板或切换工作区、代理类型后不再保留。</p> : null}
    {message ? <p role="status" className="break-words text-xs leading-5">{message}</p> : null}
    <p className="text-xs text-muted-foreground">第三方客户端请使用模型 auto。同一工作区的会话共享此设置，应用内不会随每条消息自动切换。</p>
  </section>;
}
