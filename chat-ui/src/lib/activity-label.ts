import { t } from '@lib/utils';

import type { AgentActivity } from './chat-types';

/**
 * What the chat says the agent is doing, or nothing when something else on
 * screen already says it.
 *
 * This is a function rather than a branch inside the component because the
 * decision is the part that keeps being got wrong, and a decision that lives in
 * JSX can only be checked by building the application, installing it and
 * watching. Twice that produced a confident claim and a wrong result: once with
 * the indicator disappearing mid-turn, and once with "Running…" sitting under an
 * answer that was already arriving.
 *
 * Two rules, and they are the whole of it:
 *
 *  - It SHOWS for every state where nothing else on screen says the agent is
 *    working. That deliberately includes both `running` and `reasoning`, so the
 *    handover between them changes this string and never unmounts the element.
 *    The bug that started this was exactly that swap: an indicator removed from
 *    below the list while a reasoning block was inserted inside it, two height
 *    changes in two places for one continuous state.
 *
 *  - It is SILENT for every state where something else is already speaking: the
 *    answer's own words as they arrive, the approval card, or nothing at all
 *    once the turn is over.
 *
 * There is no default branch. A state that is not named here fails to compile,
 * rather than falling into "Running…" the way `responding` once did.
 */
export function activityLabel(
  activity: AgentActivity,
  lang?: Record<string, string>,
): string | null {
  switch (activity.kind) {
    case 'running':
      return t('agent_is_running', lang, 'Running...');
    case 'reasoning':
      return t('agent_is_thinking', lang, 'Reasoning...');
    case 'tool':
      return t('agent_is_using', lang, 'Agent is using {tool}...', { tool: activity.name });
    case 'uploading':
      return activity.detail;
    case 'responding':
      // The words are arriving. The words are the indicator.
      return null;
    case 'confirming':
      // The card below is asking; this must not claim work is happening.
      return null;
    case 'idle':
      return null;
  }
}
