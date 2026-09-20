import { apiEventStream, apiFetch } from '@/lib/services/core/api-client';
import type {
  AccountsPayload,
  AdminConfigPayload,
  AdminVerifyPayload,
  AttachmentInput,
  ConversationDetailPayload,
  ConversationsPayload,
  HealthPayload,
  JsonResult,
  VersionPayload,
  AIUsagePayload,
  ChatRunInput,
  ChatRunResult,
} from './types';

export const AdminService = {
  verify() {
    return apiFetch<AdminVerifyPayload>('/admin/verify');
  },
  login(password: string) {
    return apiFetch<JsonResult>('/admin/login', {
      method: 'POST',
      body: JSON.stringify({ password }),
    });
  },
  logout() {
    return apiFetch<JsonResult>('/admin/logout', {
      method: 'POST',
    });
  },
  getConfig() {
    return apiFetch<AdminConfigPayload>('/admin/config');
  },
  getVersion() {
    return apiFetch<VersionPayload>('/admin/version');
  },
  getHealth() {
    return apiFetch<HealthPayload>('/healthz');
  },
  getAccounts() {
    return apiFetch<AccountsPayload>('/admin/accounts');
  },
  refreshWorkspaces(email: string) {
    return apiFetch<AccountsPayload>('/admin/accounts/refresh-workspaces', { method: 'POST', body: JSON.stringify({ email }) });
  },
  getAIUsage(refresh = false) {
    return apiFetch<AIUsagePayload>(`/admin/accounts/ai-usage${refresh ? '?refresh=1' : ''}`);
  },
  async streamTestPrompt(payload: ChatRunInput, onDelta: (text: string) => void, signal: AbortSignal): Promise<ChatRunResult> {
    let text = '';
    const headers = await apiEventStream('/admin/test', { ...payload, stream: true }, (data) => {
      const event = JSON.parse(data);
      if (event.error) throw new Error(event.error.message || '生成失败');
      const delta = event.choices?.[0]?.delta?.content;
      if (typeof delta === 'string') { text += delta; onDelta(delta); }
    }, signal);
    return { conversation_id: headers.get('x-conversation-id') || payload.conversation_id || '', text };
  },
  getConversations() {
    return apiFetch<ConversationsPayload>('/admin/conversations');
  },
  getConversation(id: string, local = false) {
    return apiFetch<ConversationDetailPayload>(`/admin/conversations/${encodeURIComponent(id)}${local ? '?local=1' : ''}`);
  },
  deleteConversation(id: string) {
    return apiFetch<JsonResult>(`/admin/conversations/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    });
  },
  batchDeleteConversations(ids: string[]) {
    return apiFetch<JsonResult>('/admin/conversations/batch-delete', {
      method: 'POST',
      body: JSON.stringify({ ids }),
    });
  },
  testPrompt(payload: {
    prompt: string;
    model: string;
    use_web_search: boolean;
    attachments: AttachmentInput[];
    conversation_id?: string;
  }) {
    return apiFetch<JsonResult>('/admin/test', {
      method: 'POST',
      body: JSON.stringify(payload),
    });
  },
  updateSettings(config: JsonResult) {
    return apiFetch<JsonResult>('/admin/settings', {
      method: 'PUT',
      body: JSON.stringify({ config }),
    });
  },
  importConfig(config: JsonResult) {
    return apiFetch<JsonResult>('/admin/config/import', {
      method: 'POST',
      body: JSON.stringify({ config }),
    });
  },
  exportConfig() {
    return apiFetch<JsonResult>('/admin/config/export');
  },
  createConfigSnapshot() {
    return apiFetch<JsonResult>('/admin/config/snapshot', { method: 'POST' });
  },
  listConfigSnapshots() {
    return apiFetch<JsonResult>('/admin/config/snapshot');
  },
  startAccountLogin(email: string) {
    return apiFetch<JsonResult>('/admin/accounts/login/start', {
      method: 'POST',
      body: JSON.stringify({ email }),
    });
  },
  verifyAccountCode(email: string, code: string) {
    return apiFetch<JsonResult>('/admin/accounts/login/verify', {
      method: 'POST',
      body: JSON.stringify({ email, code }),
    });
  },
  importAccount(payload: JsonResult) {
    return apiFetch<JsonResult>('/admin/accounts/manual', {
      method: 'POST',
      body: JSON.stringify(payload),
    });
  },
  quickTestAccount(payload: JsonResult) {
    return apiFetch<JsonResult>('/admin/accounts/test', {
      method: 'POST',
      body: JSON.stringify(payload),
    });
  },
  activateAccount(email: string, workspaceId?: string) {
    return apiFetch<JsonResult>('/admin/accounts/activate', {
      method: 'POST',
      body: JSON.stringify({ email, workspace_id: workspaceId }),
    });
  },
  deleteAccount(email: string) {
    return apiFetch<JsonResult>(`/admin/accounts/${encodeURIComponent(email)}`, {
      method: 'DELETE',
    });
  },
  saveAccountSettings(payload: JsonResult) {
    return apiFetch<JsonResult>('/admin/accounts', {
      method: 'PUT',
      body: JSON.stringify(payload),
    });
  },
};
