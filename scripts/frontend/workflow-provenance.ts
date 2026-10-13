/** Checks the producing workflow at its SHA; called by artifact verifier; pins the explicit producer container and update step. */
import { PLAYWRIGHT_IMAGE } from './baseline-provenance';
type Step = { run?: string; uses?: string; with?: Record<string, unknown>; 'continue-on-error'?: boolean };
type Producer = { container?: { image?: string }; env?: Record<string, string>; steps?: Step[]; 'continue-on-error'?: boolean };
export function assertProducerWorkflow(path: string, source: string) {
  if (path !== '.github/workflows/checks.yml') throw new Error('Unexpected golden producer workflow');
  const workflow = Bun.YAML.parse(source) as { jobs?: Record<string, Producer> };
  const producer = workflow.jobs?.['frontend-baselines'];
  if (producer?.container?.image !== PLAYWRIGHT_IMAGE || producer.env?.PLAYWRIGHT_IMAGE !== PLAYWRIGHT_IMAGE ||
      producer.env?.OPENSITE_BASELINE_UPDATE !== '1' || !producer.steps?.some(step => step.run === 'bun run visual:update')) {
    throw new Error('Golden producer did not use the explicit pinned container/update step');
  }
  const steps = producer.steps!;
  const writers = steps.filter(step => step.run === 'bun run visual:update');
  const tail = steps.slice(steps.findIndex(step => step.run === 'bun run visual:update') + 1);
  if (producer['continue-on-error'] || writers.length !== 1 || writers[0]['continue-on-error'] || tail.length !== 1 ||
      tail[0].run || tail[0].uses !== 'actions/upload-artifact@b7c566a772e6b6bfb58ed0dc250532a479d7789f' ||
      tail[0].with?.path !== 'tests/frontend/baselines' ||
      tail[0].with?.name !== 'frontend-baselines-${{ github.run_id }}' || tail[0].with?.['if-no-files-found'] !== 'error') {
    throw new Error('Only the pinned artifact upload may follow the final baseline writer; no post-writer mutation');
  }
}
