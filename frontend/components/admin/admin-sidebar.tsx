'use client';
import { Braces, History, KeyRound, LayoutDashboard, LogOut, Settings2, Sparkles } from 'lucide-react';
import { Dialog, DialogContent, DialogTitle } from '@/components/ui/dialog';
import type { TabKey } from '@/lib/services/admin/types';

const pages = [
  { key: 'tester', label: '聊天', icon: Sparkles },
  { key: 'accounts', label: '账号', icon: KeyRound },
  { key: 'conversations', label: '会话', icon: History },
  { key: 'dashboard', label: '状态', icon: LayoutDashboard },
  { key: 'models', label: '模型', icon: Braces },
  { key: 'settings', label: '设置', icon: Settings2 },
] as const;
type Props = {
  activeTab: TabKey; onTabChange: (tab: TabKey) => void; authState: string;
  defaultModel?: string; spaceName?: string; activeAccount?: string; onLogout: () => void;
  mobileOpen: boolean; onMobileOpenChange: (open: boolean) => void;
};
export function AdminSidebar(props: Props) {
  const content = <div className="app-navigation">
    <div className="app-brand"><span>N</span><div>Notion2API<small>工作空间</small></div></div>
    <nav aria-label="主导航">
      {pages.map(({ key, label, icon: Icon }) => <button key={key} type="button"
        aria-current={props.activeTab === key ? 'page' : undefined}
        onClick={() => { props.onTabChange(key); props.onMobileOpenChange(false); }}>
        <Icon size={17} /><span>{label}</span>
      </button>)}
    </nav>
    <div className="app-navigation-context">
      <span className="text-xs text-muted-foreground">当前工作区</span>
      <p title={props.spaceName}>{props.spaceName || '尚未选择工作区'}</p>
      <small title={props.activeAccount}>{props.activeAccount || '添加账号后开始使用'}</small>
      <small>默认模型 · {props.defaultModel || 'Auto'}</small>
    </div>
    <div className="app-navigation-footer"><span>{props.authState}</span>
      <button type="button" onClick={props.onLogout} aria-label="退出控制台"><LogOut size={15} />退出</button>
    </div>
  </div>;
  return <>
    <aside className="app-sidebar hidden lg:block">{content}</aside>
    <Dialog open={props.mobileOpen} onOpenChange={props.onMobileOpenChange}>
      <DialogContent aria-describedby={undefined} className="!left-0 !top-0 !h-dvh !w-[260px] !max-w-[90vw] !translate-x-0 !translate-y-0 rounded-none p-0">
        <DialogTitle className="sr-only">导航菜单</DialogTitle>{content}
      </DialogContent>
    </Dialog>
  </>;
}
