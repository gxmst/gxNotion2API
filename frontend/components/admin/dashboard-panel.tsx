import type { AccountsPayload, AdminConfigPayload, HealthPayload, VersionPayload } from '@/lib/services/admin/types';
import { CheckCircle2, XCircle } from 'lucide-react';
import { InfoCard, KeyValueGrid, PanelHeader, StatCard } from '@/components/admin/shared';
import { Badge } from '@/components/ui/badge';

function featureBadge(value: boolean | string, trueLabel = '开启', falseLabel = '关闭') {
  if (typeof value === 'boolean') {
    return value ? (
      <Badge variant="success" className="gap-1">
        <CheckCircle2 className="size-3" />
        {trueLabel}
      </Badge>
    ) : (
      <Badge variant="outline" className="gap-1 text-muted-foreground">
        <XCircle className="size-3" />
        {falseLabel}
      </Badge>
    );
  }
  return <Badge variant="secondary" className="font-mono normal-case">{value}</Badge>;
}

export function DashboardPanel({
  configPayload,
  versionPayload,
  healthPayload,
  accountsPayload,
}: {
  configPayload: AdminConfigPayload | null;
  versionPayload: VersionPayload | null;
  healthPayload: HealthPayload | null;
  accountsPayload: AccountsPayload | null;
}) {
  const session = configPayload?.session;
  const runtime = configPayload?.session_refresh_runtime;
  const models = configPayload?.models || [];
  const features = configPayload?.config?.features || {};
  const featureLines: Array<[string, boolean | string]> = [
    ['默认联网', Boolean(features.use_web_search)],
    ['只读模式', Boolean(features.use_read_only_mode)],
    ['强制关闭“可以进行更改”', features.force_disable_upstream_edits !== false],
    ['Writer Mode', Boolean(features.writer_mode)],
    ['图片生成', Boolean(features.enable_generate_image)],
    ['CSV 附件', Boolean(features.enable_csv_attachment_support)],
    ['AI Surface', String(features.ai_surface || 'ai_module')],
    ['Thread Type', String(features.thread_type || 'workflow')],
  ];

  const accounts = accountsPayload?.items || [];
  const workspaces = accounts.flatMap((account) => (account.workspaces || []).map((workspace) => ({ account, workspace })));
  const eligible = workspaces.filter(({ account, workspace }) => !account.disabled && workspace.eligible);
  const failures = accounts.filter((account) => account.last_error);
  return <div className="space-y-6">
    <PanelHeader eyebrow="Overview" title="运行状态" description="查看服务、账号与工作区的最近状态。点击右上角同步可更新数据。" />
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <StatCard label="服务状态" value={healthPayload?.session_ready ? '已就绪' : healthPayload?.ok ? '等待登录' : '未知'} hint={versionPayload?.version || '版本未知'} />
      <StatCard label="账号" value={String(accounts.length)} hint={accounts.filter((item) => !item.disabled).length + ' 个已启用'} />
      <StatCard label="套餐准入工作区" value={String(eligible.length)} hint="额度与模型限制仍由 Notion 决定" />
      <StatCard label="记录了错误的账号" value={String(failures.length)} hint="最近错误不一定代表当前故障" />
    </div>
    {runtime?.last_error ? <div className="app-error" role="alert">最近登录态刷新失败：{runtime.last_error}</div> : null}
    <InfoCard title="工作区概览" description="账号开关、套餐准入与模型选择能力。">
      {workspaces.length ? <div className="divide-y">{workspaces.map(({ account, workspace }) => <div key={account.email + ':' + workspace.id} className="flex flex-wrap items-center justify-between gap-3 py-4">
        <div className="min-w-0"><p className="text-sm font-medium break-all">{workspace.name || workspace.id}</p><p className="text-xs text-muted-foreground mt-1 break-all">{account.email}</p></div>
        <div className="flex flex-wrap gap-2">
          <Badge variant="outline">{account.disabled ? '账号已禁用' : workspace.eligible ? '套餐符合条件' : '待确认套餐'}</Badge>
          <Badge variant="secondary">{workspace.model_capabilities?.mode === 'manual' ? '可选择模型' : workspace.model_capabilities?.mode === 'auto_only' ? '仅 Auto' : '模型能力未知'}</Badge>
        </div>
      </div>)}</div> : <p className="text-sm text-muted-foreground">还没有工作区。请在账号页添加账号并刷新工作区信息。</p>}
    </InfoCard>
    {failures.length ? <InfoCard title="最近账号错误"><div className="space-y-4">{failures.map((item) => <div key={item.email} className="text-sm"><p className="font-medium">{item.email}</p><p className="text-muted-foreground break-all mt-1">{item.last_error}</p></div>)}</div></InfoCard> : null}
    <details className="account-tools"><summary>协议与配置详情</summary>
      <KeyValueGrid items={[
        { label: '用户', value: session?.user_email }, { label: '工作区', value: session?.space_name },
        { label: '客户端版本', value: session?.client_version }, { label: '最近刷新', value: runtime?.last_refresh_at },
        { label: 'User ID', value: session?.user_id }, { label: 'Space ID', value: session?.space_id },
        { label: 'Probe', value: session?.probe_path }, { label: '模型目录数量', value: models.length },
      ]} />
      <div className="grid gap-3 sm:grid-cols-2 mt-4">{featureLines.map(([label, value]) => <div key={label} className="flex justify-between items-center text-xs p-3 border rounded-lg"><span>{label}</span>{featureBadge(value)}</div>)}</div>
    </details>
  </div>;
}
