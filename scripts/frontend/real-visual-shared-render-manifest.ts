/** Required VISUAL-250 state render groups and PNG collision gate; consumed by CI and tests. */
import { REAL_VISUAL_STATES } from './real-visual-manifest';

type State = typeof REAL_VISUAL_STATES[250][number];
type LoadingState = Extract<State, `${string}-loading`>;
export const SHARED_RENDER: Partial<Record<LoadingState, string>> = {
  'history-loading': 'history-loading-skeleton',
  'history-detail-loading': 'history-loading-skeleton',
  'history-pagination-loading': 'history-loading-skeleton',
};

export function assertDistinctHistoryPngs(pngs: Record<string, string>) {
  for (const state of Object.keys(SHARED_RENDER)) {
    if (!(REAL_VISUAL_STATES[250] as readonly string[]).includes(state) || !state.endsWith('-loading'))
      throw new Error(`Invalid shared render state: ${state}`);
  }
  const groups: Partial<Record<State, string>> = SHARED_RENDER;
  for (const viewport of ['desktop', 'mobile']) {
    const seen = new Map<string, State>();
    for (const state of REAL_VISUAL_STATES[250]) {
      const hash = pngs[`${viewport}/real-250-${state}.png`];
      if (!hash) throw new Error(`${viewport} missing image for ${state}`);
      const previous = seen.get(hash);
      if (previous && (!groups[state] || groups[state] !== groups[previous]))
        throw new Error(`${viewport} images for ${previous} and ${state} must be distinct`);
      seen.set(hash, state);
    }
  }
}

/** Staff states use distinct fixtures and distinct error/guard renderings. No data-state aliases. */
export function assertDistinctStaffPngs(pngs: Record<string, string>) {
  for (const viewport of ['desktop', 'mobile']) {
    const hashes = REAL_VISUAL_STATES[230].map(state => pngs[`${viewport}/real-230-${state}.png`]);
    if (hashes.some(hash => !hash) || new Set(hashes).size !== hashes.length)
      throw new Error(`${viewport} staff state images must exist and be distinct`);
  }
}
