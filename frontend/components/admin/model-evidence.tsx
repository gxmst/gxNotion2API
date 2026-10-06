import type { ConversationMessage, ModelItem } from '@/lib/services/admin/types';
import { modelDisplayName } from '@/lib/model-display';

export function ModelEvidence({ message, models = [], preferredModels = [] }: { message: ConversationMessage; models?: ModelItem[]; preferredModels?: ModelItem[] }) {
  if (message.role === 'user' || message.status === 'streaming') return null;
  const observations = message.model_observations || [];
  const labels = [...new Set(observations.map((item) => {
    return modelDisplayName(item.model, models, item.provider, preferredModels) || `未识别模型（${item.model}）`;
  }))];
  return <details className="model-evidence" data-testid="model-evidence">
    <summary title="查看上游模型信息">实际模型：{labels.length ? labels.join(' / ') : '未知（上游未报告）'}
      {message.model_selection_mode === 'auto_fallback' ? <span> · Auto 兼容</span> : message.model_selection_mode === 'auto' ? <span> · Auto 分配</span> : null}
    </summary>
    <div className="model-evidence-details">
      {observations.map((item, index) => <p key={index}>上游代号：{item.model}{item.provider ? ` · ${item.provider}` : ''} · 来源：{item.source}</p>)}
      {message.model_selection_mode === 'auto_fallback' ? <p>请求模型：{modelDisplayName(message.requested_model || '', models) || message.requested_model}，已按 Auto 兼容执行</p> : null}
      {!observations.length ? <p>上游未返回实际模型信息。</p> : null}
    </div>
  </details>;
}
