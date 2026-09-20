import type { ConversationMessage, ModelItem } from '@/lib/services/admin/types';

export function ModelEvidence({ message, models = [] }: { message: ConversationMessage; models?: ModelItem[] }) {
  if (message.role === 'user' || message.status === 'streaming') return null;
  const observations = message.model_observations || [];
  const labels = [...new Set(observations.map((item) => {
    const model = models.find((model) => model.notion_model === item.model);
    return `${model?.name || item.model}${item.provider ? ` · ${item.provider}` : ''}`;
  }))];
  return <div className="mt-2 text-xs leading-5 text-muted-foreground" data-testid="model-evidence">
    <span title={observations.map((item) => `${item.model} (${item.source})`).join(', ')}>实际模型：{labels.length ? labels.join(' / ') : '未知（上游未报告）'}</span>
    {message.model_selection_mode === 'auto_fallback' ? <span> · 请求 {message.requested_model}，已按 Auto 兼容执行</span> : message.model_selection_mode === 'auto' ? <span> · Auto 分配</span> : null}
  </div>;
}
