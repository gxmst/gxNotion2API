import { apiEventStream, apiFetch } from '@/lib/services/core/api-client';
import type {
  AccountsPayload,
  AdminConfigPayload,
  AdminVerifyPayload,
  AttachmentInput,
  ConversationDetailPayload,
  ConversationMutationPayload,
  ConversationsPayload,
  HealthPayload,
  JsonResult,
  VersionPayload,
  AIUsagePayload,
  WorkspaceAIUsagePayload,
  ChatRunInput,
  ChatRunResult,
  ModelPolicySnapshot,
  ModelPolicyEdit,
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
  refreshModels(email: string, workspace_id: string) {
    return apiFetch('/admin/accounts/refresh-models', { method: 'POST', body: JSON.stringify({ email, workspace_id }) });
  },
  getModelPolicy(email: string, workspace_id: string, scope: 'personal' | 'custom') {
    const query = new URLSearchParams({ email, workspace_id, scope });
    return apiFetch<ModelPolicySnapshot>(`/admin/accounts/model-policy?${query}`, { cache: 'no-store' });
  },
  updateModelPolicy(payload: ModelPolicyEdit) {
    return apiFetch<ModelPolicySnapshot>('/admin/accounts/model-policy', { method: 'PUT', body: JSON.stringify(payload), redirect: 'error' });
  },
  getAIUsage(refresh = false) {
    return apiFetch<AIUsagePayload>(`/admin/accounts/ai-usage${refresh ? '?refresh=1' : ''}`);
  },
  getWorkspaceAIUsage(email: string, workspaceId: string, refresh = false) {
    const query = new URLSearchParams({ email, workspace_id: workspaceId });
    if (refresh) query.set('refresh', '1');
    return apiFetch<WorkspaceAIUsagePayload>(`/admin/accounts/ai-usage/workspace?${query}`, { cache: 'no-store' });
  },
  async streamTestPrompt(payload: ChatRunInput, onDelta: (text: string) => void, signal: AbortSignal): Promise<ChatRunResult> {
    let text = '';
    let truncated = false;
    const headers = await apiEventStream('/admin/test', { ...payload, stream: true }, (data) => {
      const event = JSON.parse(data);
      if (event.error) throw new Error(event.error.message || '生成失败');
      const choice = event.choices?.[0];
      // A length stop means the upstream stream ended before the answer did, so
      // the transcript must say so instead of presenting it as complete.
      if (choice?.finish_reason === 'length') truncated = true;
      const delta = choice?.delta?.content;
      if (typeof delta === 'string') { text += delta; onDelta(delta); }
    }, signal);
    return { conversation_id: headers.get('x-conversation-id') || payload.conversation_id || '', text, truncated };
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
  renameConversation(id: string, title: string) {
    return apiFetch<ConversationMutationPayload>(`/admin/conversations/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify({ title }),
    });
  },
  editConversationMessage(id: string, messageId: string, content: string) {
    return apiFetch<ConversationMutationPayload>(
      `/admin/conversations/${encodeURIComponent(id)}/messages/${encodeURIComponent(messageId)}`,
      { method: 'PATCH', body: JSON.stringify({ content }) },
    );
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
