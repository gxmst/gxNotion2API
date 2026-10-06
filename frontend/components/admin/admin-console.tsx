'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTheme } from 'next-themes';
import {
  AlertTriangle,
  LoaderCircle,
  Menu,
  RefreshCcw,
} from 'lucide-react';
import { AccountsPanel } from '@/components/admin/accounts-panel';
import { AdminSidebar } from '@/components/admin/admin-sidebar';
import { ConversationsPanel } from '@/components/admin/conversations-panel';
import { DashboardPanel } from '@/components/admin/dashboard-panel';
import { toast } from 'sonner';
import { LoginOverlay } from '@/components/admin/login-overlay';
import { ModelsPanel } from '@/components/admin/models-panel';
import { SettingsPanel } from '@/components/admin/settings-panel';
import { ChatWorkspace } from '@/components/admin/chat-workspace';
import { Button } from '@/components/ui/button';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { useAdminConsole } from '@/hooks/use-admin-console';
import type { TabKey } from '@/lib/services/admin/types';

const TAB_LABEL: Record<TabKey, string> = {
  dashboard: '状态',
  tester: '聊天',
  conversations: '会话',
  settings: '设置',
  accounts: '账号',
  models: '模型',
};

export function AdminConsole() {
  const consoleState = useAdminConsole();
  const [activeTab, setActiveTabState] = useState<TabKey>('tester');
  const settingsDirty = useRef(false);
  const accountsDirty = useRef(false);
  const activeTabRef = useRef<TabKey>('tester');
  const handleSettingsDirtyChange = useCallback((dirty: boolean) => { settingsDirty.current = dirty; }, []);
  const handleAccountsDirtyChange = useCallback((dirty: boolean) => { accountsDirty.current = dirty; }, []);
  const canLeave = useCallback((tab: TabKey) => {
    if (tab === activeTabRef.current) return true;
    return !(settingsDirty.current || accountsDirty.current) || window.confirm('有未保存的修改，离开后将丢失。确定离开？');
  }, []);
  const setActiveTab = useCallback((tab: TabKey) => {
    if (!canLeave(tab)) return;
    activeTabRef.current = tab;
    setActiveTabState(tab);
    const url = new URL(window.location.href);
    url.searchParams.set('tab', tab);
    window.history.pushState(null, '', url);
  }, [canLeave]);
  useEffect(() => {
    const sync = () => {
      const value = new URL(window.location.href).searchParams.get('tab');
      const tab = value && Object.prototype.hasOwnProperty.call(TAB_LABEL, value) ? value as TabKey : 'tester';
      if (!canLeave(tab)) {
        const url = new URL(window.location.href);
        url.searchParams.set('tab', activeTabRef.current);
        window.history.replaceState(null, '', url);
        return;
      }
      activeTabRef.current = tab;
      setActiveTabState(tab);
    };
    sync();
    window.addEventListener('popstate', sync);
    return () => window.removeEventListener('popstate', sync);
  }, [canLeave]);
  const [resumeConversationID, setResumeConversationID] = useState('');
  const [loginBusy, setLoginBusy] = useState(false);
  const [loginMessage, setLoginMessage] = useState('');
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const { theme, setTheme } = useTheme();

  const {
    verify,
    configPayload,
    versionPayload,
    healthPayload,
    accountsPayload,
    conversations,
    selectedConversation,
    selectedConversationId,
    streamState,
    bootLoading,
    bootError,
    setSelectedConversationId,
    loadConversationDetail,
    loadConversations,
    refreshConversations,
    refreshAll,
    refreshAccounts,
    refreshConfigBundle,
    login,
    logout,
    deleteConversation,
    batchDeleteConversations,
    services,
  } = consoleState;

  const authenticated = Boolean(verify?.authenticated);
  const passwordConfigured = Boolean(verify?.password_configured);
  const shouldShowOverlay = Boolean(verify) && !authenticated;
  const models = configPayload?.models || [];
  const defaultModel =
    configPayload?.config?.default_model ||
    configPayload?.config?.model_id ||
    versionPayload?.default_model;
  const defaultWebSearch = Boolean(configPayload?.config?.features?.use_web_search);

  const authState = useMemo(() => {
    if (authenticated) return '已认证';
    if (!verify?.admin_enabled) return '未启用';
    if (!passwordConfigured) return '未配置密码';
    return '待登录';
  }, [authenticated, passwordConfigured, verify?.admin_enabled]);

  const themeMode = theme === 'light' || theme === 'dark' || theme === 'system' ? theme : 'system';

  const renderActivePanel = () => {
    switch (activeTab) {
      case 'tester': return null;
      case 'conversations':
        return (
          <ConversationsPanel
            models={models}
            conversations={conversations}
            selectedConversationId={selectedConversationId}
            selectedConversation={selectedConversation}
            streamState={streamState}
            onRefresh={refreshConversations}
            onSelect={async (conversationId) => {
              setSelectedConversationId(conversationId);
              await loadConversationDetail(conversationId, false);
            }}
            onDelete={deleteConversation}
            onBatchDelete={batchDeleteConversations}
            onContinue={(id) => { setResumeConversationID(id); setActiveTab('tester'); }}
          />
        );
      case 'settings':
        return configPayload ? (
          <SettingsPanel
            config={configPayload.config}
            models={models}
            adminPasswordSet={Boolean(configPayload.secrets?.admin_password_set)}
            onDirtyChange={handleSettingsDirtyChange}
            onSave={async (config) => {
              const payload = await services.updateSettings(config);
              await refreshAll();
              return payload;
            }}
            onImport={async (config) => {
              const payload = await services.importConfig(config);
              await refreshAll();
              return payload;
            }}
            onExport={services.exportConfig}
            onCreateSnapshot={services.createConfigSnapshot}
            onListSnapshot={services.listConfigSnapshots}
            onTestPrompt={async (payload) => {
              const result = await services.testPrompt(payload);
              await loadConversations();
              return result;
            }}
          />
        ) : null;
      case 'accounts':
        return (
          <AccountsPanel
            onDirtyChange={handleAccountsDirtyChange}
            accountsPayload={accountsPayload}
            models={models}
            defaultModel={defaultModel}
            onRefresh={refreshAccounts}
            onRefreshWorkspaces={async (email) => { await services.refreshWorkspaces(email); await refreshAccounts(); await refreshConfigBundle(); }}
            onRefreshModels={async (email, workspaceID) => { await services.refreshModels(email, workspaceID); await refreshAccounts(); await refreshConfigBundle(); }}
            onStartLogin={async (email) => {
              const payload = await services.startAccountLogin(email);
              await refreshAccounts();
              await refreshConfigBundle();
              return payload;
            }}
            onVerifyCode={async (email, code) => {
              const payload = await services.verifyAccountCode(email, code);
              await refreshAll();
              return payload;
            }}
            onImportAccount={async (payload) => {
              const result = await services.importAccount(payload);
              await refreshAll();
              return result;
            }}
            onQuickTest={async (payload) => {
              const result = await services.quickTestAccount(payload);
              await loadConversations();
              return result;
            }}
            onActivate={async (email, workspaceId) => {
              const payload = await services.activateAccount(email, workspaceId);
              await refreshAll();
              return payload;
            }}
            onDelete={async (email) => {
              const payload = await services.deleteAccount(email);
              await refreshAll();
              return payload;
            }}
            onSaveAccountSettings={async (payload) => {
              const result = await services.saveAccountSettings(payload);
              await refreshAccounts();
              await refreshConfigBundle();
              return result;
            }}
          />
        );
      case 'models':
        return <ModelsPanel onOpenAccounts={() => setActiveTab('accounts')} models={models} defaultModel={defaultModel} />;
      case 'dashboard':
      default:
        return (
          <DashboardPanel
            configPayload={configPayload}
            versionPayload={versionPayload}
            healthPayload={healthPayload}
            accountsPayload={accountsPayload}
          />
        );
    }
  };


  if (bootLoading && !verify) return <main className="app-loading"><LoaderCircle className="size-5 animate-spin" /><p>正在加载工作空间…</p></main>;

  return (
    <main className="app-shell console-surface">
      <AdminSidebar activeTab={activeTab} onTabChange={setActiveTab} authState={authState}
        defaultModel={defaultModel} spaceName={configPayload?.session?.space_name || accountsPayload?.session?.space_name}
        activeAccount={accountsPayload?.active_account} mobileOpen={mobileNavOpen} onMobileOpenChange={setMobileNavOpen}
        onLogout={() => {
          if ((settingsDirty.current || accountsDirty.current) && !window.confirm('有未保存的修改，确定退出？')) return;
          void logout().catch((error) => toast.error(error instanceof Error ? error.message : '退出失败'));
        }} />
      <section className="app-main">
        <header className="app-topbar">
          <Button variant="ghost" size="icon" className="lg:hidden" aria-label="打开导航菜单" onClick={() => setMobileNavOpen(true)}><Menu className="size-4" /></Button>
          <span className="font-medium text-sm">{TAB_LABEL[activeTab]}</span>
          <span className="app-topbar-status hidden md:inline">{streamState}</span>
          <div className="ml-auto flex items-center gap-2">
            <Select value={themeMode} onValueChange={setTheme}>
              <SelectTrigger size="sm" className="w-[112px]" aria-label="主题模式"><SelectValue /></SelectTrigger>
              <SelectContent align="end"><SelectItem value="system">跟随系统</SelectItem><SelectItem value="light">浅色</SelectItem><SelectItem value="dark">深色</SelectItem></SelectContent>
            </Select>
            <Button variant="ghost" size="icon" aria-label="重新同步" disabled={bootLoading} onClick={() => void refreshAll()}>
              <RefreshCcw className={bootLoading ? 'size-4 animate-spin' : 'size-4'} />
            </Button>
          </div>
        </header>
        {bootError ? <div role="alert" className="app-error"><AlertTriangle size={16} />{bootError}</div> : null}
        {authenticated && configPayload ? <div className="chat-root app-chat" hidden={activeTab !== 'tester'}>
        <ChatWorkspace models={models} defaultModel={defaultModel} defaultWebSearch={defaultWebSearch}
          initialConversationID={resumeConversationID} onResumeHandled={() => setResumeConversationID('')}
          conversations={conversations} accounts={accountsPayload?.items || []}
          onNavigate={setActiveTab} visible={activeTab === 'tester'}
          onLoad={(id) => services.getConversation(id, true)}
          onDeleteConversation={deleteConversation}
          onRefreshConversations={loadConversations}
          onRefreshWorkspaceModels={async (email, workspaceID) => { await services.refreshModels(email, workspaceID); await refreshAccounts(); await refreshConfigBundle(); }}
          onRun={async (payload, onDelta, signal) => {
            try { return await services.streamTestPrompt(payload, onDelta, signal); }
            finally { void loadConversations().catch(() => undefined); }
          }} />
        </div> : null}
        {authenticated && !configPayload ? <div className="app-loading"><p>{bootError || '正在加载配置…'}</p><Button onClick={() => void refreshAll()}>重新加载</Button></div> : null}
        {authenticated && activeTab !== 'tester' ? <div className="console-content">{renderActivePanel()}</div> : null}
      </section>
      {shouldShowOverlay ? <LoginOverlay passwordConfigured={passwordConfigured}
        message={loginMessage || bootError || (passwordConfigured ? '请输入密码登录。' : '未检测到 admin.password。')}
        busy={loginBusy} onSubmit={async (password) => {
          setLoginBusy(true); setLoginMessage('');
          try { await login(password); }
          catch (error) { setLoginMessage(error instanceof Error ? error.message : '登录失败'); throw error; }
          finally { setLoginBusy(false); }
        }} /> : null}
    </main>
  );
}
