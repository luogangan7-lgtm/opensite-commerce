/** Responsibility: retry a failed SSR request; dependencies: React/shared Button.
 * Callers: error template island; invariant: no API secrets or tenant data enter browser fetches.
 */
import { Button } from '@opensite/ui';
export default function Retry({ label }: { label: string }) {
  return <Button onClick={() => window.location.reload()}>{label}</Button>;
}
