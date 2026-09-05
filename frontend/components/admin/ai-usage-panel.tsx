'use client';

import { useEffect, useState } from 'react';
import { RefreshCcw } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { AdminService } from '@/lib/services/admin/admin.service';
import type { AIUsagePayload } from '@/lib/services/admin/types';

export function AIUsagePanel() {
  const [payload, setPayload] = useState<AIUsagePayload | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  async function refresh() {
    setLoading(true); setError('');
    try { setPayload(await AdminService.getAIUsage(true)); }
    catch (cause) { setError(cause instanceof Error ? cause.message : '额度查询失败'); }
    finally { setLoading(false); }
  }
  useEffect(() => {
    let cancelled = false;
    void AdminService.getAIUsage().then((result) => { if (!cancelled) setPayload(result); })
      .catch((cause) => { if (!cancelled) setError(cause instanceof Error ? cause.message : '额度查询失败'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, []);
  return (
    <section className="space-y-3 border-y py-5">
      <div className="flex items-center justify-between gap-3">
        <h2 className="text-base font-semibold">Notion 额度</h2>
        <Button size="icon" variant="ghost" disabled={loading} onClick={() => void refresh()} title="刷新 Notion 额度" aria-label="刷新 Notion 额度"><RefreshCcw className={`size-4 ${loading ? 'animate-spin' : ''}`} /></Button>
      </div>
      {error ? <p role="status" className="text-sm text-destructive">{error}</p> : null}
      {!payload && loading ? <p className="text-sm text-muted-foreground">查询中...</p> : null}
      <div className="overflow-x-auto">
        <table className="w-full min-w-[680px] text-left text-sm">
          <thead><tr className="border-b text-xs text-muted-foreground"><th className="py-2 pr-4">账号 / 工作区</th><th className="px-2 py-2">基础额度</th><th className="px-2 py-2">工作区用量</th><th className="px-2 py-2">成员用量</th><th className="px-2 py-2">当前周期</th><th className="px-2 py-2">Credits 余额</th><th className="px-2 py-2">查询时间</th></tr></thead>
          <tbody>{payload?.accounts.map((report) => {
            const usage = report.usage;
            const enforced = usage?.quota_enforced;
            const status = report.status !== 'ok' ? '未知' : !usage?.is_eligible_known ? '资格未知' : !usage.is_eligible ? '不可用' : enforced ? '有限额' : usage.type === 'unlimited' ? '基础限额未启用' : '限额未知';
            return <tr key={`${report.email}-${report.space_id}`} className="border-b last:border-0">
              <td className="max-w-64 py-3 pr-4"><div className="break-all">{report.email}</div><div className="mt-1 break-all text-xs text-muted-foreground">{report.space_id || '工作区未知'}</div>{report.detail ? <p className="mt-1 break-words text-xs text-destructive">{report.detail}</p> : null}</td>
              <td className="px-2 py-3">{status}</td>
              <td className="px-2 py-3 tabular-nums">{usage?.basic_usage_known ? `${usage.space_usage}${enforced && usage.basic_limits_known ? ` / ${usage.space_limit}` : ''}` : '未知'}</td>
              <td className="px-2 py-3 tabular-nums">{usage?.basic_usage_known ? `${usage.user_usage}${enforced && usage.basic_limits_known ? ` / ${usage.user_limit}` : ''}` : '未知'}</td>
              <td className="px-2 py-3 text-xs tabular-nums">{usage?.current_period_usage_known ? <><div>工作区 {usage.current_period_space_usage ?? 0}</div><div>成员 {usage.current_period_user_usage ?? 0}</div></> : '未知'}</td>
              <td className="px-2 py-3 tabular-nums">{usage?.premium_credit_known ? <><div>{usage.premium_credit_balance ?? 0}</div>{usage.credits_in_overage ? <div className="text-xs text-muted-foreground">超额 {usage.credits_in_overage}</div> : null}</> : '未知'}</td>
              <td className="px-2 py-3 text-xs text-muted-foreground">{report.fetched_at ? new Date(report.fetched_at).toLocaleString() : '-'}{report.cached ? ' (缓存)' : ''}</td>
            </tr>;
          })}</tbody>
        </table>
      </div>
    </section>
  );
}
