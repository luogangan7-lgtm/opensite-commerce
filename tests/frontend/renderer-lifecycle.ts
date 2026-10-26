/**
 * Responsibility: bounded lifecycle for the standalone SSR renderer spawned by
 * tests/frontend/storefront.spec.ts.
 * Dependencies: node:child_process stdio only.
 * Invariants: the terminal exit/error promise is registered immediately after
 * spawn (Node does not guarantee 'exit' after 'error'); readiness races
 * termination and reports the original start error with the last 4 KiB of
 * stderr; cleanup never waits for an event that may already have fired
 * (SIGTERM, bounded wait, SIGKILL, bounded wait).
 */

import { spawn, type ChildProcess } from 'node:child_process';
import type { Readable } from 'node:stream';

export const STDERR_TAIL_BYTES = 4 * 1024;
export const READY_TIMEOUT_MS = 10_000;
const DEFAULT_TERM_TIMEOUT_MS = 5_000;
const DEFAULT_KILL_TIMEOUT_MS = 5_000;
const READY_INTERVAL_MS = 100;
const STDERR_DRAIN_MS = 500;

export type RendererTerminal =
  | { readonly kind: 'exit'; readonly code: number | null; readonly signal: NodeJS.Signals | null }
  | { readonly kind: 'error'; readonly error: Error };

export interface RendererOptions {
  readonly command: string;
  readonly args?: readonly string[];
  readonly env?: NodeJS.ProcessEnv;
  readonly cwd?: string;
  readonly stderrLimitBytes?: number;
  readonly termTimeoutMs?: number;
  readonly killTimeoutMs?: number;
}

export interface ReadyOptions {
  readonly timeoutMs?: number;
  readonly intervalMs?: number;
}

export interface RendererHandle {
  readonly child: ChildProcess;
  /** Settles once on the first of 'exit' or 'error'; never rejects. */
  readonly terminal: Promise<RendererTerminal>;
  hasExited(): boolean;
  stderrTail(): string;
  /** Resolves when `probe` is true; rejects if the child terminates or the bound elapses first. */
  waitForReady(probe: () => boolean | Promise<boolean>, options?: ReadyOptions): Promise<void>;
  /** Bounded TERM then KILL cleanup; safe to call after the child already exited. */
  stop(): Promise<void>;
}

/** Resolve once `stream` ends (or closes), but never wait longer than `ms`. */
async function drain(stream: Readable | null, ms: number): Promise<void> {
  if (!stream || stream.readableEnded || stream.destroyed) return;
  await new Promise<void>(resolve => {
    const timer = setTimeout(resolve, ms);
    const finish = () => {
      clearTimeout(timer);
      resolve();
    };
    stream.once('end', finish);
    stream.once('close', finish);
  });
}

export function spawnRenderer(options: RendererOptions): RendererHandle {
  const child = spawn(options.command, [...(options.args ?? [])], {
    cwd: options.cwd,
    env: options.env,
    stdio: ['ignore', 'ignore', 'pipe'],
  });
  const limit = options.stderrLimitBytes ?? STDERR_TAIL_BYTES;
  const termTimeoutMs = options.termTimeoutMs ?? DEFAULT_TERM_TIMEOUT_MS;
  const killTimeoutMs = options.killTimeoutMs ?? DEFAULT_KILL_TIMEOUT_MS;

  const chunks: Buffer[] = [];
  let tailBytes = 0;
  const stderr = child.stderr;
  stderr?.on('data', (chunk: Buffer) => {
    chunks.push(chunk);
    tailBytes += chunk.length;
    while (tailBytes > limit && chunks.length > 1) tailBytes -= chunks.shift()!.length;
    if (tailBytes > limit) {
      chunks[0] = chunks[0]!.subarray(tailBytes - limit);
      tailBytes = limit;
    }
  });

  let terminalState: RendererTerminal | undefined;
  const terminal = new Promise<RendererTerminal>(resolve => {
    const settle = (value: RendererTerminal) => {
      if (terminalState) return;
      terminalState = value;
      resolve(value);
    };
    // Registered immediately: after 'error' Node does not guarantee 'exit'.
    child.once('exit', (code, signal) => settle({ kind: 'exit', code, signal }));
    child.once('error', error => settle({ kind: 'error', error }));
  });

  const hasExited = () => terminalState !== undefined;
  const stderrTail = () => Buffer.concat(chunks).toString('utf8');
  const diagnostics = () => `\n--- renderer stderr (last ${limit} bytes) ---\n${stderrTail().trimEnd() || '<empty>'}`;

  /** The start error shown instead of a lost exit event or an opaque timeout. */
  const startFailure = async (): Promise<Error> => {
    await drain(stderr, STDERR_DRAIN_MS);
    const state = terminalState!;
    const detail = state.kind === 'error'
      ? `failed to start: ${state.error.message}`
      : `exited before ready (code ${state.code ?? 'null'}, signal ${state.signal ?? 'null'})`;
    return new Error(`renderer '${options.command}' ${detail}${diagnostics()}`);
  };

  /** True if the terminal promise settles within `ms`; never waits for an event twice. */
  const settledWithin = async (ms: number): Promise<boolean> => {
    if (terminalState) return true;
    return await new Promise<boolean>(resolve => {
      const timer = setTimeout(() => resolve(false), ms);
      void terminal.then(() => {
        clearTimeout(timer);
        resolve(true);
      });
    });
  };

  const waitForReady = async (probe: () => boolean | Promise<boolean>, ready: ReadyOptions = {}): Promise<void> => {
    const timeoutMs = ready.timeoutMs ?? READY_TIMEOUT_MS;
    const intervalMs = ready.intervalMs ?? READY_INTERVAL_MS;
    const deadline = Date.now() + timeoutMs;
    let probeError: unknown;
    for (;;) {
      if (terminalState) throw await startFailure();
      // Race the probe against termination so a dead child fails ready-wait immediately.
      const raced = await Promise.race([
        Promise.resolve().then(probe).then(value => ({ kind: 'probe' as const, value }), error => ({ kind: 'probe-error' as const, error })),
        terminal.then(() => ({ kind: 'terminated' as const })),
      ]);
      if (terminalState) throw await startFailure();
      if (raced.kind === 'probe' && raced.value) return;
      if (raced.kind === 'probe-error') probeError = raced.error;
      const remaining = deadline - Date.now();
      if (remaining <= 0) {
        await drain(stderr, STDERR_DRAIN_MS);
        const reason = probeError instanceof Error ? ` (last probe error: ${probeError.message})` : '';
        throw new Error(`renderer '${options.command}' not ready after ${timeoutMs}ms${reason}${diagnostics()}`);
      }
      await Promise.race([
        new Promise<void>(resolve => setTimeout(resolve, Math.min(intervalMs, remaining))),
        terminal.then(() => undefined),
      ]);
    }
  };

  const stop = async (): Promise<void> => {
    if (terminalState) {
      await terminal;
      return;
    }
    child.kill('SIGTERM');
    if (await settledWithin(termTimeoutMs)) return;
    child.kill('SIGKILL');
    if (await settledWithin(killTimeoutMs)) return;
    await drain(stderr, STDERR_DRAIN_MS);
    throw new Error(`renderer '${options.command}' survived SIGKILL for ${killTimeoutMs}ms${diagnostics()}`);
  };

  return { child, terminal, hasExited, stderrTail, waitForReady, stop };
}
