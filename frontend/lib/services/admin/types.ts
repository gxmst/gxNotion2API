export type TabKey = 'dashboard' | 'tester' | 'conversations' | 'settings' | 'accounts' | 'models';

export interface ModelItem {
  default_reasoning_effort?: string;
  supported_reasoning_efforts?: string[];
  disabled_reason?: string;
  id: string;
  name?: string;
  family?: string;
  group?: string;
  notion_model?: string;
  beta?: boolean;
  enabled?: boolean;
}

export interface WorkspaceModelPolicy {
  disabledModels: string[];
  disabledProviders: string[];
}

export interface ModelPolicyRestorePoint {
  policy: WorkspaceModelPolicy;
  present: boolean;
  saved_at: string;
}

export interface ModelPolicySnapshot {
  email: string;
  workspace_id: string;
  scope: 'personal' | 'custom';
  membership_type: string;
  can_edit: boolean;
  policy: WorkspaceModelPolicy;
  policy_present: boolean;
  revision: string;
  models: { id: string; name: string; provider: string; available: boolean; disabled_reason?: string; allowed: boolean }[];
  // Server-persisted pre-change policy captured at the first successful lock;
  // cleared after a successful restore or clear.
  restore_point?: ModelPolicyRestorePoint | null;
}

export interface ModelPolicyEdit {
  email: string;
  workspace_id: string;
  scope: 'personal' | 'custom';
  revision: string;
  action: 'lock' | 'restore' | 'clear';
  model_id?: string;
}

export interface SessionSummary {
  user_email?: string;
  user_name?: string;
  space_name?: string;
  space_id?: string;
  probe_path?: string;
  client_version?: string;
  user_id?: string;
}

export interface SessionRefreshRuntime {
  last_refresh_at?: string;
  last_error?: string;
}

export interface FeatureConfig {
  use_web_search?: boolean;
  use_read_only_mode?: boolean;
  force_disable_upstream_edits?: boolean;
  force_fresh_thread_per_request?: boolean;
  allow_native_transport_fallback?: boolean;
  writer_mode?: boolean;
  enable_generate_image?: boolean;
  enable_csv_attachment_support?: boolean;
  ai_surface?: string;
  thread_type?: string;
  search_scopes?: string[];
  [key: string]: unknown;
}

export interface PromptConfig {
  profile?: string;
  custom_prefix?: string;
  fallback_profiles?: string[];
  max_escalation_steps?: number;
  max_refusal_retries?: number;
  cognitive_reframing_prefix?: string;
  toolbox_capability_expansion_prefix?: string;
  coding_retry_prefixes?: string[];
  general_retry_prefixes?: string[];
  direct_answer_retry_prefixes?: string[];
}

export interface AppConfigShape {
  host?: string;
  port?: number;
  api_key?: string;
  upstream_base_url?: string;
  upstream_origin?: string;
  upstream_host_header?: string;
  upstream_tls_server_name?: string;
  upstream_use_env_proxy?: boolean;
  timeout_sec?: number;
  poll_interval_sec?: number;
  poll_max_rounds?: number;
  stream_chunk_runes?: number;
  default_model?: string;
  model_id?: string;
  debug_upstream?: boolean;
  model_aliases?: Record<string, string>;
  responses?: {
    store_ttl_seconds?: number;
  };
  storage?: {
    sqlite_path?: string;
    persist_conversations?: boolean;
    persist_conversation_snapshots?: boolean;
    persist_responses?: boolean;
    persist_continuation_sessions?: boolean;
    persist_sillytavern_bindings?: boolean;
  };
  admin?: {
    password?: string;
    token_ttl_hours?: number;
  };
  login_helper?: {
    sessions_dir?: string;
    timeout_sec?: number;
  };
  prompt?: PromptConfig;
  features?: FeatureConfig;
}

export interface AdminConfigPayload {
  success: boolean;
  config: AppConfigShape;
  secrets?: {
    api_key_set?: boolean;
    admin_password_set?: boolean;
  };
  session?: SessionSummary;
  session_refresh_runtime?: SessionRefreshRuntime;
  models?: ModelItem[];
}

export interface AdminVerifyPayload {
  authenticated?: boolean;
  password_required?: boolean;
  password_configured?: boolean;
  admin_enabled?: boolean;
}

export interface VersionPayload {
  success?: boolean;
  version?: string;
  name?: string;
  default_model?: string;
  model_count?: number;
  user_email?: string;
  space_id?: string;
  features?: FeatureConfig;
  responses?: {
    store_ttl_seconds?: number;
  };
  storage?: {
    persist_conversations?: boolean;
    persist_conversation_snapshots?: boolean;
    persist_responses?: boolean;
    persist_continuation_sessions?: boolean;
    persist_sillytavern_bindings?: boolean;
  };
}

export interface HealthPayload {
  ok?: boolean;
  session_ready?: boolean;
  version?: string;
  model?: string;
  user_email?: string;
  space_id?: string;
}

export interface AccountItem {
  credential_cooldown_until?: string;
  credential_cooldown_active?: boolean;
  account_max_concurrency?: number;
  email?: string;
  active?: boolean;
  disabled?: boolean;
  status?: string;
  last_login_at?: string;
  last_success_at?: string;
  last_refresh_at?: string;
  last_relogin_at?: string;
  priority?: number;
  hourly_quota?: number;
  max_concurrency?: number;
  quota_limited?: boolean;
  remaining_quota?: number;
  cooldown_active?: boolean;
  cooldown_remaining_sec?: number;
  cooldown_until?: string;
  window_started_at?: string;
  window_request_count?: number;
  total_successes?: number;
  total_failures?: number;
  consecutive_failures?: number;
  user_id?: string;
  user_name?: string;
  space_id?: string;
  space_view_id?: string;
  space_name?: string;
  plan_type?: string;
  default_workspace_id?: string;
  workspaces?: WorkspaceItem[];
  client_version?: string;
  probe_json?: string;
  probe_exists?: boolean;
  profile_dir?: string;
  profile_dir_exists?: boolean;
  storage_state_path?: string;
  storage_state_exists?: boolean;
  pending_state_path?: string;
  pending_state_exists?: boolean;
  last_used_at?: string;
  last_error?: string;
  login_status?: {
    status?: string;
    message?: string;
    error?: string;
  };
}

export interface WorkspaceItem {
  model_capabilities?: { mode: 'manual' | 'auto_only' | 'unknown'; checked_at?: string; models?: ModelItem[]; catalog?: ModelItem[] } | null;
  ai_disabled?: boolean;
  eligible?: boolean;
  eligibility_reason?: string;
  id: string;
  view_id?: string;
  name?: string;
  plan_type?: string;
  subscription_tier?: string;
  ai_enabled?: boolean;
  status?: string;
  last_error?: string;
  priority?: number;
  hourly_quota?: number;
  max_concurrency?: number;
  quota_limited?: boolean;
  remaining_quota?: number;
  window_started_at?: string;
  window_request_count?: number;
  cooldown_until?: string;
  cooldown_active?: boolean;
  cooldown_remaining_sec?: number;
  last_used_at?: string;
  last_success_at?: string;
  last_refresh_at?: string;
  last_quota_exhausted_at?: string;
  consecutive_failures?: number;
  total_successes?: number;
  total_failures?: number;
  default?: boolean;
  active?: boolean;
}

export interface AccountsPayload {
  items?: AccountItem[];
  active_account?: string;
  active_workspace_id?: string;
  session_ready?: boolean;
  session?: SessionSummary;
  login_helper?: {
    sessions_dir?: string;
    timeout_sec?: number;
  };
  session_refresh?: {
    enabled?: boolean;
    interval_minutes?: number;
  };
  session_refresh_runtime?: SessionRefreshRuntime;
}

export interface AIUsageReport {
  email: string;
  space_id?: string;
  workspace_name?: string;
  status: string;
  detail?: string;
  cached: boolean;
  fetched_at?: string;
  usage?: {
    is_eligible: boolean;
    is_eligible_known: boolean;
    type?: string;
    quota_enforced: boolean;
    basic_usage_known?: boolean;
    basic_limits_known?: boolean;
    space_usage: number;
    space_limit: number;
    user_usage: number;
    user_limit: number;
    current_period_usage_known?: boolean;
    current_period_space_usage?: number;
    current_period_user_usage?: number;
    promotional_usage_known?: boolean;
    promotional_usage?: number;
    promotional_limit_known?: boolean;
    promotional_limit?: number;
    premium_credit_known?: boolean;
    premium_credit_balance?: number;
    credits_in_overage?: number;
    overage_limit?: number;
    premium_service_period_start_ms?: number;
    rate_limit?: AIUsageRateLimit;
  };
}

/**
 * One rolling allowance window as Notion reports it. `label` is upstream's own
 * name for the window ("6h", "billing_period", ...) and is displayed verbatim;
 * `used`/`limit` are raw counters, so a remaining percentage is derived only
 * when `limit > 0`.
 */
export interface AIUsageRateLimitWindow {
  label?: string;
  credit_type?: string;
  scope?: string;
  cadence?: string;
  used: number;
  limit: number;
  period_end_ms?: number;
}

export interface AIUsageRateLimit {
  status?: string;
  short?: AIUsageRateLimitWindow;
  long?: AIUsageRateLimitWindow;
}

export interface AIUsagePayload { accounts: AIUsageReport[]; ttl_seconds: number }

export interface WorkspaceAIUsagePayload { report: AIUsageReport; ttl_seconds: number }

export interface ChatRunInput {
  account_email?: string;
  workspace_id?: string;
  prompt: string;
  model: string;
  use_web_search: boolean;
  attachments: AttachmentInput[];
  conversation_id?: string;
}

export interface ChatRunResult { conversation_id: string; text: string }

export interface ConversationMessageAttachment {
  name?: string;
  content_type?: string;
  contentType?: string;
  /** Set for files Notion itself shared, so the transcript can link to them. */
  url?: string;
  source?: string;
}

export interface ConversationMessage {
  requested_model?: string;
  model_selection_mode?: string;
  model_observations?: { model: string; provider?: string; source: string; step_id?: string }[];
  id?: string;
  role?: 'user' | 'assistant' | string;
  status?: string;
  content?: string;
  created_at?: string;
  updated_at?: string;
  /** Set when an operator rewrote the text, so the UI can mark it as edited. */
  edited_at?: string;
  /**
   * Set on process entries (role "step") and on attachment steps. The raw
   * upstream type is passed through, so the UI labels it without depending on
   * a fixed list of step names.
   */
  step_type?: string;
  attachments?: ConversationMessageAttachment[];
}

/** Response of the conversation PATCH routes: the whole updated conversation. */
export interface ConversationMutationPayload {
  success: boolean;
  item: ConversationDetail;
}

export interface ConversationSummary {
  id: string;
  title?: string;
  origin?: 'local' | 'notion' | 'merged' | string;
  remote_only?: boolean;
  preview?: string;
  request_prompt?: string;
  status?: string;
  source?: string;
  transport?: string;
  model?: string;
  notion_model?: string;
  account_email?: string;
  space_id?: string;
  thread_id?: string;
  trace_id?: string;
  response_id?: string;
  completion_id?: string;
  created_by_display_name?: string;
  created_at?: string;
  updated_at?: string;
  error?: string;
}

export interface ConversationDetail extends ConversationSummary {
  messages?: ConversationMessage[];
}

export interface ConversationsPayload {
  items?: ConversationSummary[];
}

export interface ConversationDetailPayload {
  item: ConversationDetail;
}

export interface JsonResult {
  [key: string]: unknown;
}

export interface AttachmentInput {
  type: 'attachment';
  name: string;
  content_type: string;
  data: string | ArrayBuffer | null;
}
