/**
 * #415: pins the bounded renderer lifecycle used by storefront.spec.ts.
 * Regression cases: an immediate exit, a spawn error, a child that ignores
 * SIGTERM (SIGKILL only after the bounded TERM wait), a normal start/stop, a
 * stalled probe, a probe that reports ready after the deadline (both must time
 * out), and timer cleanup after termination.
 * No browser or web server is involved; caller: scripts/check_frontend.sh.
 */
import { expect, test } from 'bun:test';
import { spawnRenderer, STDERR_TAIL_BYTES } from './renderer-lifecycle';

type ReadyOutcome =
  | { readonly kind: 'resolved' }
  | { readonly kind: 'rejected'; readonly error: Error }
  | { readonly kind: 'hung' };

async function failure(run: Promise<void>): Promise<Error> {
  try {
    await run;
  } catch (error) {
    return error instanceof Error ? error : new Error(String(error));
  }
  throw new Error('expected the renderer readiness wait to fail');
}

/** Bounds a readiness wait so a hang fails this test instead of the job timeout. */
async function settleWithin(run: Promise<void>, guardMs: number): Promise<ReadyOutcome> {
  let guard: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      run.then(
        () => ({ kind: 'resolved' as const }),
        error => ({ kind: 'rejected' as const, error: error instanceof Error ? error : new Error(String(error)) }),
      ),
      new Promise<ReadyOutcome>(resolve => {
        guard = setTimeout(() => resolve({ kind: 'hung' }), guardMs);
      }),
    ]);
  } finally {
    if (guard) clearTimeout(guard);
  }
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

test('a probe that never settles times out at the bound and reports the stderr tail', async () => {
  const renderer = spawnRenderer({
    command: 'bash',
    args: ['-c', 'printf "renderer standing by\\n" >&2; while :; do sleep 0.05; done'],
  });
  const timeoutMs = 300;
  try {
    const started = Date.now();
    const outcome = await settleWithin(
      renderer.waitForReady(() => new Promise<boolean>(() => {}), { timeoutMs, intervalMs: 25 }),
      3_000,
    );
    const elapsed = Date.now() - started;
    expect(outcome.kind).toBe('rejected');
    expect(elapsed).toBeGreaterThanOrEqual(timeoutMs);
    expect(elapsed).toBeLessThan(timeoutMs + 2_000);
    if (outcome.kind !== 'rejected') {
      throw new Error(`a stalled probe must time out, got ${outcome.kind}`);
    }
    expect(outcome.error.message).toContain(`not ready after ${timeoutMs}ms`);
    expect(outcome.error.message).toContain('renderer stderr');
    expect(outcome.error.message).toContain('renderer standing by');
  } finally {
    await renderer.stop();
  }
});

test('a probe that turns true only after the deadline is reported as a timeout', async () => {
  const renderer = spawnRenderer({ command: 'bash', args: ['-c', 'while :; do sleep 0.05; done'] });
  const timeoutMs = 250;
  try {
    const started = Date.now();
    const outcome = await settleWithin(
      renderer.waitForReady(
        () => new Promise<boolean>(resolve => {
          setTimeout(() => resolve(true), timeoutMs + 150);
        }),
        { timeoutMs, intervalMs: 25 },
      ),
      3_000,
    );
    expect(outcome.kind).toBe('rejected');
    expect(Date.now() - started).toBeLessThan(timeoutMs + 1_500);
    if (outcome.kind !== 'rejected') {
      throw new Error(`an overdue ready probe must be a timeout, got ${outcome.kind}`);
    }
    expect(outcome.error.message).toContain(`not ready after ${timeoutMs}ms`);
  } finally {
    await renderer.stop();
  }
});

test('terminal settling first clears the readiness timers instead of leaking them', async () => {
  const renderer = spawnRenderer({ command: 'bash', args: ['-c', 'sleep 0.2'] });
  type TimerRecord = { readonly delay: number; cleared: boolean; fired: boolean };
  const created: TimerRecord[] = [];
  const records = new Map<unknown, TimerRecord>();
  // The cast keeps this test independent of the runtime's timer handle type.
  const globals = globalThis as unknown as {
    setTimeout: (callback: (...args: any[]) => void, ms?: number, ...args: any[]) => unknown;
    clearTimeout: (handle?: unknown) => void;
  };
  const realSetTimeout = globals.setTimeout.bind(globalThis);
  const realClearTimeout = globals.clearTimeout.bind(globalThis);
  const originalSetTimeout = globals.setTimeout;
  const originalClearTimeout = globals.clearTimeout;
  let guard: unknown;
  const guardHit = new Promise<'hung'>(resolve => {
    guard = realSetTimeout(() => resolve('hung'), 5_000);
  });
  try {
    globals.setTimeout = (callback: (...args: any[]) => void, ms?: number, ...args: any[]) => {
      const record: TimerRecord = { delay: ms ?? 0, cleared: false, fired: false };
      const handle = realSetTimeout(() => {
        record.fired = true;
        records.delete(handle);
        callback(...args);
      }, ms);
      records.set(handle, record);
      created.push(record);
      return handle;
    };
    globals.clearTimeout = (handle?: unknown) => {
      const record = handle === undefined ? undefined : records.get(handle);
      if (record) {
        record.cleared = true;
        records.delete(handle);
      }
      realClearTimeout(handle);
    };

    const started = Date.now();
    const outcome = await Promise.race([
      renderer.waitForReady(() => false, { timeoutMs: 10_000, intervalMs: 10_000 }).then(
        () => 'resolved' as const,
        (error: unknown) => ({ error }),
      ),
      guardHit,
    ]);
    expect(Date.now() - started).toBeLessThan(2_000);
    expect(outcome).not.toBe('hung');
    if (outcome === 'hung') throw new Error('readiness wait hung after the renderer exited');
    if (outcome === 'resolved') throw new Error('an exited renderer must never report ready');
    expect(outcome.error).toBeInstanceOf(Error);
    expect((outcome.error as Error).message).toContain('exited before ready');
  } finally {
    globals.setTimeout = originalSetTimeout;
    globals.clearTimeout = originalClearTimeout;
    realClearTimeout(guard);
    await renderer.stop();
  }
  // Readiness owns exactly two timers here, both bounded by this test's 10s budget
  // (the deadline race and the interval sleep); stderr drain is 500ms.
  const longLived = created.filter(record => record.delay >= 9_000 && record.delay <= 10_000);
  expect(longLived.length).toBe(2);
  expect(longLived.filter(record => !record.cleared)).toEqual([]);
  expect(longLived.filter(record => record.fired)).toEqual([]);
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
