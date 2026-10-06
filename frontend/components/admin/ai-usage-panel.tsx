'use client';

import { useEffect, useState } from 'react';
import { QuotaWindows } from '@/components/admin/quota-windows';
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
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {(payload?.accounts || []).map((report) => <div className="rounded-lg border p-4 space-y-4" key={(report.email || '') + report.space_id}>
          <div><h3 className="text-sm font-medium">{report.workspace_name || '工作区'}</h3><p className="text-xs text-muted-foreground break-all mt-1">{report.email}</p></div>
          <QuotaWindows report={report} />
          <details className="text-xs text-muted-foreground"><summary>其他用量</summary><p className="mt-2">累计基础用量：{report.usage?.basic_usage_known ? report.usage.user_usage : '未知'} · 额外积分余额：{report.usage?.premium_credit_known ? report.usage.premium_credit_balance : '未知'}</p></details>
        </div>)}
      </div>
    </section>
  );
}
