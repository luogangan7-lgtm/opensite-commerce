/** Checks the producing workflow at its SHA; called by artifact verifier; pins the explicit producer container and update step. */
import { PLAYWRIGHT_IMAGE } from './baseline-provenance';
type Producer = { container?: { image?: string }; env?: Record<string, string>; steps?: { run?: string }[] };
export function assertProducerWorkflow(path: string, source: string) {
  if (path !== '.github/workflows/checks.yml') throw new Error('Unexpected golden producer workflow');
  const workflow = Bun.YAML.parse(source) as { jobs?: Record<string, Producer> };
  const producer = workflow.jobs?.['frontend-baselines'];
  if (producer?.container?.image !== PLAYWRIGHT_IMAGE || producer.env?.PLAYWRIGHT_IMAGE !== PLAYWRIGHT_IMAGE ||
      producer.env?.OPENSITE_BASELINE_UPDATE !== '1' || !producer.steps?.some(step => step.run === 'bun run visual:update')) {
    throw new Error('Golden producer did not use the explicit pinned container/update step');
  }
}
