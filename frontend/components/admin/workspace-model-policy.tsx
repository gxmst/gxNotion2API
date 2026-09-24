'use client';

import { useEffect, useRef, useState } from 'react';
import { AlertTriangle } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { formatMaybeDate } from '@/components/admin/shared';
import { AdminService } from '@/lib/services/admin/admin.service';
import type { ModelPolicyEdit, ModelPolicySnapshot } from '@/lib/services/admin/types';

type PolicyAction = ModelPolicyEdit['action'];

const SUCCESS_MESSAGE: Record<PolicyAction, string> = {
  lock: '已确认工作区设置保存成功。聊天继续使用 Auto，实际模型以回答中的上游报告为准。',
  restore: '已恢复修改前的设置。',
  clear: '已清除模型限制，工作区恢复 Notion 默认设置。',
};

export function WorkspaceModelPolicy({ email, workspaceID, workspaceName, coolingDown, onPolicyChanged }: {
  email: string; workspaceID: string; workspaceName: string; coolingDown?: boolean;
  // Called after a successful write so model capabilities and the account list are re-read.
  onPolicyChanged?: () => Promise<unknown>;
}) {
  const [scope, setScope] = useState<'personal' | 'custom'>('personal');
  const [snapshot, setSnapshot] = useState<ModelPolicySnapshot | null>(null);
  const [modelID, setModelID] = useState('');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [confirmClear, setConfirmClear] = useState(false);
  const alive = useRef(true);
  const inFlight = useRef(false);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);

  const scopeLabel = scope === 'personal' ? '普通 Notion 代理' : '自定义代理';

  function adopt(value: ModelPolicySnapshot) {
    setSnapshot(value);
    const allowed = value.models.filter((model) => model.allowed);
    setModelID(allowed.length === 1 ? allowed[0].id : '');
  }

  async function read() {
    if (inFlight.current || coolingDown) return;
    inFlight.current = true;
    setBusy(true); setMessage('');
    try {
      const value = await AdminService.getModelPolicy(email, workspaceID, scope);
      if (!alive.current) return;
      adopt(value);
    } catch (error) {
      if (alive.current) { setSnapshot(null); setMessage(error instanceof Error ? error.message : '读取失败'); }
    } finally { inFlight.current = false; if (alive.current) setBusy(false); }
  }

  async function apply(action: PolicyAction) {
    if (inFlight.current || coolingDown || !snapshot?.can_edit) return;
    if (action === 'lock' && !modelID) return;
    if (action === 'restore' && !snapshot.restore_point) return;
    inFlight.current = true;
    setBusy(true); setMessage('');
    try {
      let value: ModelPolicySnapshot;
      try {
        value = await AdminService.updateModelPolicy({ email, workspace_id: workspaceID, scope, revision: snapshot.revision, action,
          ...(action === 'lock' ? { model_id: modelID } : {}),
        });
      } catch (error) {
        // Whether or not the write landed, the local snapshot is no longer
        // trustworthy; the restore point lives on the server, so a reread
        // shows it either way.
        if (alive.current) { setSnapshot(null); setMessage(`${error instanceof Error ? error.message : '保存失败'} 请先重新读取设置。`); }
        return;
      }
      if (!alive.current) return;
      adopt(value);
      if (!onPolicyChanged) { setMessage(SUCCESS_MESSAGE[action]); return; }
      setMessage(`${SUCCESS_MESSAGE[action]} 正在刷新模型能力…`);
      try {
        await onPolicyChanged();
        if (alive.current) setMessage(`${SUCCESS_MESSAGE[action]} 模型能力已刷新。`);
      } catch (error) {
        if (alive.current) setMessage(`${SUCCESS_MESSAGE[action]} 模型能力刷新失败：${error instanceof Error ? error.message : '未知错误'}，请手动点击「刷新模型能力」。`);
      }
    } finally { inFlight.current = false; if (alive.current) setBusy(false); }
  }

  const selected = snapshot?.models.find((model) => model.id === modelID);
  const allowed = snapshot?.models.filter((model) => model.allowed) || [];
  const editable = snapshot?.can_edit && !coolingDown;
  const restorePoint = snapshot?.restore_point || null;
  const noUsableModel = Boolean(snapshot?.policy_present) && allowed.length === 0;
  return <section aria-label="工作区模型设置" className="col-span-full space-y-3 rounded-lg border p-3 text-sm">
    <div><h3 className="font-medium">工作区模型设置</h3><p className="mt-1 break-words text-xs text-muted-foreground">{workspaceName} · {email}</p></div>
    <p className="text-xs leading-5 text-muted-foreground">仅在点击应用、恢复或清除时修改 Notion 设置。修改影响整个工作区中此类代理的所有会话和成员；只有工作区所有者可操作。</p>
    <Select value={scope} disabled={busy} onValueChange={(value: 'personal' | 'custom') => {
      if (inFlight.current) return;
      setScope(value); setSnapshot(null); setModelID(''); setMessage('');
    }}><SelectTrigger aria-label="代理类型"><SelectValue /></SelectTrigger><SelectContent>
      <SelectItem value="personal">普通 Notion 代理</SelectItem><SelectItem value="custom">自定义代理</SelectItem>
    </SelectContent></Select>
    <Button variant="outline" disabled={busy || coolingDown} onClick={() => void read()}>{busy ? '正在处理…' : '读取模型设置'}</Button>
    {coolingDown ? <p className="text-xs text-muted-foreground">账号冷却期间暂停读取和修改。</p> : null}
    {snapshot ? <>
      <p className="text-xs">当前权限：{snapshot.membership_type === 'owner' ? '工作区所有者' : snapshot.membership_type === 'unknown' ? '未确认' : snapshot.membership_type}。{!snapshot.can_edit ? '当前仅可查看，无法修改。' : ''}</p>
      {noUsableModel ? <div role="alert" className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs leading-5 text-destructive">
        <AlertTriangle className="mt-0.5 size-4 shrink-0" />
        <span>当前限制下目录内没有任何允许的模型，此工作区的{scopeLabel}将无可用模型。请选择一个可用模型应用，或恢复修改前设置、清除限制。</span>
      </div> : null}
      <p className="text-xs leading-5 text-muted-foreground">当前目录内允许：{allowed.length ? allowed.map((model) => model.name).join('、') : '没有可用模型'}。{!snapshot.policy_present ? '尚未设置额外模型限制。' : ''}</p>
      <Select value={modelID} onValueChange={setModelID} disabled={busy || !editable}>
        <SelectTrigger aria-label="工作区目标模型"><SelectValue placeholder="选择希望 Auto 使用的模型" /></SelectTrigger>
        <SelectContent>{snapshot.models.map((model) => <SelectItem key={model.id} value={model.id} disabled={!model.available}>
          {model.name}{!model.available ? `（不可用${model.disabled_reason ? `：${model.disabled_reason}` : ''}）` : ''}
        </SelectItem>)}</SelectContent>
      </Select>
      <p className="text-xs leading-5 text-muted-foreground">应用后将允许目标模型，并排除当前目录内的其他模型与供应商。目录新增模型后需重新确认；此设置不会解除试用或套餐限制。</p>
      {selected ? <p className="text-xs">将应用到「{workspaceName}」的{scopeLabel}：{selected.name}</p> : null}
      <div className="flex flex-wrap gap-2">
        <Button disabled={busy || !editable || !selected?.available} onClick={() => void apply('lock')}>应用到整个工作区</Button>
        <Button variant="outline" disabled={busy || !editable || !restorePoint} onClick={() => void apply('restore')}>恢复修改前设置</Button>
        <Button variant="outline" className="text-destructive hover:text-destructive" disabled={busy || !editable || !snapshot.policy_present} onClick={() => setConfirmClear(true)}>清除限制</Button>
      </div>
      {restorePoint ? <p className="text-xs leading-5 text-muted-foreground">
        服务器已保存修改前设置（{formatMaybeDate(restorePoint.saved_at)}）：{restorePoint.present
          ? `排除 ${restorePoint.policy?.disabledModels?.length ?? 0} 个模型、${restorePoint.policy?.disabledProviders?.length ?? 0} 个供应商`
          : '当时未设置额外限制'}。恢复或清除成功后此恢复点会被删除。
      </p> : <p className="text-xs text-muted-foreground">暂无恢复点；首次成功应用模型时，服务器会保存当时的设置。</p>}
    </> : null}
    {message ? <p role="status" className="break-words text-xs leading-5">{message}</p> : null}
    <p className="text-xs text-muted-foreground">第三方客户端请使用模型 auto。同一工作区的会话共享此设置，应用内不会随每条消息自动切换。</p>
    <Dialog open={confirmClear} onOpenChange={setConfirmClear}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>清除工作区模型限制？</DialogTitle>
          <DialogDescription>
            将删除「{workspaceName}」{scopeLabel}的模型限制，恢复 Notion 默认设置（目录内所有模型均允许）。此操作影响整个工作区中此类代理的所有会话和成员，并会删除服务器保存的恢复点。
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={() => setConfirmClear(false)}>取消</Button>
          <Button variant="destructive" disabled={busy} onClick={() => { setConfirmClear(false); void apply('clear'); }}>确认清除</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </section>;
}
