/**
 * #415: pins the bounded renderer lifecycle used by storefront.spec.ts.
 * Regression cases: an immediate exit, a spawn error, a child that ignores
 * SIGTERM (SIGKILL only after the bounded TERM wait), and a normal start/stop.
 * No browser or web server is involved; caller: scripts/check_frontend.sh.
 */
import { expect, test } from 'bun:test';
import { spawnRenderer, STDERR_TAIL_BYTES } from './renderer-lifecycle';

async function failure(run: Promise<void>): Promise<Error> {
  try {
    await run;
  } catch (error) {
    return error instanceof Error ? error : new Error(String(error));
  }
  throw new Error('expected the renderer readiness wait to fail');
}

test('immediate exit fails readiness fast with the original exit code and stderr tail', async () => {
  const renderer = spawnRenderer({
    command: 'bash',
    args: ['-c', 'printf "renderer entry missing: dist/server/entry.mjs\\n" >&2; exit 7'],
  });
  try {
    const started = Date.now();
    const error = await failure(renderer.waitForReady(async () => false, { timeoutMs: 10_000, intervalMs: 25 }));
    expect(Date.now() - started).toBeLessThan(5_000);
    expect(error.message).toContain('exited before ready');
    expect(error.message).toContain('code 7');
    expect(error.message).toContain('renderer entry missing: dist/server/entry.mjs');
  } finally {
    await renderer.stop();
  }
});

test('spawn error rejects readiness with the original start error instead of waiting for exit', async () => {
  const renderer = spawnRenderer({ command: 'osc-415-renderer-binary-that-does-not-exist' });
  try {
    const started = Date.now();
    const error = await failure(renderer.waitForReady(async () => false, { timeoutMs: 10_000, intervalMs: 25 }));
    expect(Date.now() - started).toBeLessThan(5_000);
    expect(error.message).toContain('failed to start');
    expect(error.message).toMatch(/ENOENT|not found/);
  } finally {
    await renderer.stop();
  }
});

test('a renderer that ignores SIGTERM is SIGKILLed only after the bounded TERM wait', async () => {
  const termTimeoutMs = 200;
  const renderer = spawnRenderer({
    command: 'bash',
    args: ['-c', 'trap "" TERM; printf "ignoring term\\n" >&2; while :; do sleep 0.05; done'],
    termTimeoutMs,
    killTimeoutMs: 5_000,
  });
  try {
    await renderer.waitForReady(async () => renderer.stderrTail().includes('ignoring term'), { timeoutMs: 5_000, intervalMs: 25 });
    const started = Date.now();
    await renderer.stop();
    expect(Date.now() - started).toBeGreaterThanOrEqual(termTimeoutMs);
    expect(await renderer.terminal).toMatchObject({ kind: 'exit', signal: 'SIGKILL' });
  } finally {
    await renderer.stop();
  }
});

test('a normal start and stop exits cleanly on SIGTERM', async () => {
  const renderer = spawnRenderer({
    command: 'bash',
    args: ['-c', 'trap "exit 0" TERM; printf "ready\\n" >&2; while :; do sleep 0.05; done'],
    termTimeoutMs: 5_000,
    killTimeoutMs: 5_000,
  });
  try {
    await renderer.waitForReady(async () => renderer.stderrTail().includes('ready'), { timeoutMs: 5_000, intervalMs: 25 });
    expect(renderer.hasExited()).toBe(false);
    await renderer.stop();
    expect(await renderer.terminal).toEqual({ kind: 'exit', code: 0, signal: null });
  } finally {
    await renderer.stop();
  }
});

test('start failure diagnostics keep only the last bounded stderr bytes', async () => {
  const renderer = spawnRenderer({
    command: 'bash',
    args: ['-c', 'for i in $(seq 1 800); do printf "noise-%04d\\n" "$i" >&2; done; printf "FINAL-START-ERROR\\n" >&2; exit 9'],
  });
  try {
    const error = await failure(renderer.waitForReady(async () => false, { timeoutMs: 10_000, intervalMs: 25 }));
    expect(Buffer.byteLength(renderer.stderrTail())).toBeLessThanOrEqual(STDERR_TAIL_BYTES);
    expect(error.message).toContain('FINAL-START-ERROR');
    expect(error.message).not.toContain('noise-0001');
  } finally {
    await renderer.stop();
  }
});
