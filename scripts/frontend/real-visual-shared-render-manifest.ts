/** Required VISUAL-250 state render groups and PNG collision gate; consumed by CI and tests. */
import { REAL_VISUAL_STATES } from './real-visual-manifest';

type State = typeof REAL_VISUAL_STATES[250][number];
export const SHARED_RENDER: Partial<Record<State, string>> = {
  'history-loading': 'history-loading-skeleton',
  'history-detail-loading': 'history-loading-skeleton',
  'history-pagination-loading': 'history-loading-skeleton',
};

export function assertDistinctHistoryPngs(pngs: Record<string, string>) {
  for (const viewport of ['desktop', 'mobile']) {
    const seen = new Map<string, State>();
    for (const state of REAL_VISUAL_STATES[250]) {
      const hash = pngs[`${viewport}/real-250-${state}.png`];
      if (!hash) throw new Error(`${viewport} missing image for ${state}`);
      const previous = seen.get(hash);
      if (previous && (!SHARED_RENDER[state] || SHARED_RENDER[state] !== SHARED_RENDER[previous]))
        throw new Error(`${viewport} images for ${previous} and ${state} must be distinct`);
      seen.set(hash, state);
    }
  }
}
