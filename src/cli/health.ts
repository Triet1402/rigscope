/**
 * Bounded health probes for the managed CLI.
 *
 * A successful `systemctl start` is not evidence that the activated release is
 * healthy. These probes verify the actual listener and, when the service
 * reports a version, compare it with the version that was staged. They never
 * send browser credentials, collector tokens or request bodies.
 */

import { request } from 'node:http';
import { connect as netConnect } from 'node:net';
import { connect as tlsConnect } from 'node:tls';

export type HealthTarget =
  | { kind: 'loopback'; host: string; port: number; path: string }
  | { kind: 'unix'; socketPath: string; hostHeader: string; path: string };

export type HealthProbe = {
  ok: boolean;
  statusCode: number | null;
  body: Record<string, unknown> | null;
  error: string | null;
};

export function healthTarget(config: {
  role?: 'local' | 'hub' | 'collector';
  component?: 'hub' | 'collector';
  port: number;
  origin?: string;
  socket?: string;
}): HealthTarget {
  const collector = config.component ? config.component === 'collector' : config.role === 'collector';
  if (collector) throw new Error('collector role has no hub health endpoint');
  if (config.origin) {
    const origin = new URL(config.origin);
    return {
      kind: 'unix',
      socketPath: config.socket ?? '',
      hostHeader: origin.host,
      path: '/api/health',
    };
  }
  return { kind: 'loopback', host: '127.0.0.1', port: config.port, path: '/api/health' };
}

const MAX_HEALTH_BODY_BYTES = 64 * 1024;

export function probeHealth(target: HealthTarget, timeoutMs = 3_000): Promise<HealthProbe> {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (result: HealthProbe): void => {
      if (settled) return;
      settled = true;
      resolve(result);
    };
    const options =
      target.kind === 'unix'
        ? { socketPath: target.socketPath, path: target.path, method: 'GET', headers: { Host: target.hostHeader } }
        : { host: target.host, port: target.port, path: target.path, method: 'GET' };
    let req: ReturnType<typeof request>;
    try {
      req = request(options, (res) => {
        const chunks: Buffer[] = [];
        let bytes = 0;
        res.on('data', (chunk: Buffer) => {
          bytes += chunk.length;
          if (bytes > MAX_HEALTH_BODY_BYTES) {
            req.destroy();
            finish({ ok: false, statusCode: res.statusCode ?? null, body: null, error: 'health response is oversized' });
            return;
          }
          chunks.push(chunk);
        });
        res.on('end', () => {
          const statusCode = res.statusCode ?? null;
          const text = Buffer.concat(chunks).toString('utf8');
          let body: Record<string, unknown> | null = null;
          try {
            const value: unknown = JSON.parse(text);
            if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
              body = value as Record<string, unknown>;
            }
          } catch {
            // A non-JSON body is reported as unhealthy below.
          }
          const ok = statusCode === 200 && body?.status === 'ok';
          finish({
            ok,
            statusCode,
            body,
            error: ok ? null : statusCode === 200 ? 'health response is not ok' : `health returned HTTP ${statusCode ?? 'unknown'}`,
          });
        });
      });
    } catch (error) {
      finish({ ok: false, statusCode: null, body: null, error: errorText(error) });
      return;
    }
    const deadline = setTimeout(() => {
      req.destroy();
      finish({ ok: false, statusCode: null, body: null, error: `health probe timed out after ${timeoutMs}ms` });
    }, timeoutMs);
    req.once('close', () => {
      clearTimeout(deadline);
      finish({ok:false, statusCode:null, body:null, error:'health connection closed before a complete response'});
    });
    req.on('error', (error) => {
      finish({ ok: false, statusCode: null, body: null, error: errorText(error) });
    });
    req.end();
  });
}

export type WaitForHealthResult = {
  ok: boolean;
  attempts: number;
  elapsedMs: number;
  last: HealthProbe;
  version: string | null;
};

/**
 * Poll until the deadline. The timeout is for the whole wait, not each attempt,
 * so a hung request cannot extend an upgrade indefinitely.
 */
export async function waitForHealth(
  target: HealthTarget,
  options: { deadlineMs?: number; intervalMs?: number; expectedVersion?: string; probeTimeoutMs?: number } = {},
): Promise<WaitForHealthResult> {
  const deadlineMs = bounded(options.deadlineMs ?? 20_000, 100, 120_000);
  const intervalMs = bounded(options.intervalMs ?? 250, 25, 5_000);
  const probeTimeoutMs = bounded(options.probeTimeoutMs ?? 3_000, 100, 30_000);
  const started = Date.now();
  let attempts = 0;
  let last: HealthProbe = { ok: false, statusCode: null, body: null, error: 'health was not probed' };
  while (Date.now() - started < deadlineMs) {
    attempts += 1;
    const remaining = Math.max(100, deadlineMs - (Date.now() - started));
    last = await probeHealth(target, Math.min(probeTimeoutMs, remaining));
    const reported = typeof last.body?.version === 'string' ? last.body.version : null;
    const versionMatches = options.expectedVersion === undefined || reported === options.expectedVersion;
    if (last.ok && versionMatches) {
      return { ok: true, attempts, elapsedMs: Date.now() - started, last, version: reported };
    }
    if (last.ok && !versionMatches) {
      last = { ...last, ok: false, error: `running version ${reported} does not match expected ${options.expectedVersion}` };
    }
    const wait = Math.min(intervalMs, Math.max(0, deadlineMs - (Date.now() - started)));
    if (wait > 0) await delay(wait);
  }
  return { ok: false, attempts, elapsedMs: Date.now() - started, last, version: null };
}

export type ConnectivityProbe = { ok: boolean; error: string | null; ms: number };

/** Certificate-validating TLS reachability probe. No HTTP request is sent. */
export function probeTls(hostname: string, port: number, timeoutMs = 3_000): Promise<ConnectivityProbe> {
  const started = Date.now();
  return new Promise((resolve) => {
    let settled = false;
    const finish = (ok: boolean, error: string | null): void => {
      if (settled) return;
      settled = true;
      resolve({ ok, error, ms: Date.now() - started });
    };
    let socket: ReturnType<typeof tlsConnect>;
    try {
      socket = tlsConnect({
        host: hostname,
        port,
        servername: hostname,
        rejectUnauthorized: true,
        timeout: timeoutMs,
      });
    } catch (error) {
      finish(false, errorText(error));
      return;
    }
    socket.once('secureConnect', () => {
      socket.end();
      finish(true, null);
    });
    socket.once('error', (error) => finish(false, errorText(error)));
    socket.once('timeout', () => {
      socket.destroy();
      finish(false, `TLS connection timed out after ${timeoutMs}ms`);
    });
  });
}

export function probeTcp(host: string, port: number, timeoutMs = 3_000): Promise<ConnectivityProbe> {
  const started = Date.now();
  return new Promise((resolve) => {
    let settled = false;
    const finish = (ok: boolean, error: string | null): void => {
      if (settled) return;
      settled = true;
      resolve({ ok, error, ms: Date.now() - started });
    };
    let socket: ReturnType<typeof netConnect>;
    try {
      socket = netConnect({ host, port });
    } catch (error) {
      finish(false, errorText(error));
      return;
    }
    socket.setTimeout(timeoutMs);
    socket.once('connect', () => {
      socket.destroy();
      finish(true, null);
    });
    socket.once('timeout', () => {
      socket.destroy();
      finish(false, `TCP connection timed out after ${timeoutMs}ms`);
    });
    socket.once('error', (error) => finish(false, errorText(error)));
  });
}

function bounded(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, Number.isFinite(value) ? value : min));
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
