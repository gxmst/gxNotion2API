import type { ModelItem } from '@/lib/services/admin/types';

// Display-only evidence from the October 6 Notion settings capture. These
// labels never enable a model or participate in request routing.
const verifiedNames: Record<string, { name: string; provider: string }> = {
  'albuquerque-quinn': { name: 'Opus 5.5', provider: 'anthropic' },
  'achira-donut': { name: 'Sonnet 5.5', provider: 'anthropic' },
};
const key = (value?: string) => (value || '').trim().toLowerCase();
const providers = new Set(['anthropic', 'openai', 'gemini', 'deepseek', 'kimi', 'glm', 'xai']);

export function modelDisplayName(raw: string, models: ModelItem[], provider?: string, preferred: ModelItem[] = []) {
  function matches(catalog: ModelItem[]) {
    return catalog.filter((model) => {
      if (providers.has(key(provider)) && providers.has(key(model.family)) && key(provider) !== key(model.family)) return false;
      return [model.id, model.notion_model, ...(model.aliases || [])].some((alias) => key(alias) === key(raw));
    });
  }
  const scoped = matches(preferred);
  const candidates = scoped.length ? scoped : matches(models);
  const names = [...new Set(candidates.map((model) => model.name?.trim()).filter((name): name is string => Boolean(name) && key(name) !== key(raw)))];
  if (names.length === 1) return names[0];
  // An ambiguous live catalog must not be overridden by an older known name.
  if (names.length > 1) return null;
  const verified = verifiedNames[key(raw)];
  if (verified && (!provider || key(provider) === verified.provider)) return verified.name;
  return candidates.some((model) => key(model.name) === key(raw)) ? raw : null;
}
