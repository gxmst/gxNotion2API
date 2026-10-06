'use client';
import { useEffect, useState } from 'react';
import type { AIUsageReport, AIUsageRateLimitWindow } from '@/lib/services/admin/types';

function usedPercent(window: AIUsageRateLimitWindow) {
  if (!Number.isFinite(window.used) || !Number.isFinite(window.limit) || window.limit <= 0) return null;
  return Math.max(0, Math.min(100, Math.round(window.used / window.limit * 10000) / 100));
}
function resetLabel(window: AIUsageRateLimitWindow, rolling: boolean, now: number) {
  if (!rolling) return window.period_end_ms ? '重置于 ' + new Date(window.period_end_ms).toLocaleDateString('zh-CN', { month: 'long', day: 'numeric' }) : '重置日期未返回';
  if (!window.resets_at_ms) return '滚动周期 ' + (window.label || '未知') + ' · 上游尚未返回重置时间';
  const seconds = Math.max(0, Math.floor((window.resets_at_ms - now) / 1000));
  if (!seconds) return '已到重置时间，等待刷新确认';
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor(seconds % 3600 / 60);
  return (hours ? hours + ' 小时 ' : '') + (minutes ? minutes + ' 分钟' : hours ? '内' : '不到 1 分钟') + '后重置';
}

export function QuotaWindows({ report }: { report: AIUsageReport }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 15_000);
    return () => window.clearInterval(timer);
  }, []);
  const rate = report.usage?.rate_limit;
  return <div className="space-y-4">
    {report.status !== 'ok' ? <p className="text-xs text-muted-foreground">{report.detail || '额度暂时不可用'}</p> : null}
    {([['滚动', rate?.short], ['月度', rate?.long]] as const).map(([label, window]) => {
      const percent = window ? usedPercent(window) : null;
      return <div key={label} className="space-y-2">
        <div className="flex items-center justify-between gap-3 text-xs"><strong className="font-medium">{label}</strong><span>{percent === null ? '使用量未知' : '已使用 ' + percent + '%'}</span></div>
        <div role="progressbar" aria-label={label + '额度使用量'} aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent ?? undefined} className="h-1.5 overflow-hidden rounded-full bg-muted">
          <div className={percent !== null && percent >= 85 ? 'h-full bg-destructive' : 'h-full bg-primary'} style={{ width: (percent ?? 0) + '%' }} />
        </div>
        <p className="text-[11px] leading-5 text-muted-foreground">{window ? resetLabel(window, label === '滚动', now) : '上游未返回此窗口'}</p>
      </div>;
    })}
    <p className="text-[10px] leading-5 text-muted-foreground">{report.fetched_at ? '更新于 ' + new Date(report.fetched_at).toLocaleTimeString('zh-CN') : ''}{report.cached ? ' · 缓存' : ''} · 重置倒计时在本地更新</p>
  </div>;
}
