import type { IncomingMessage, ServerResponse } from 'node:http';
import type { HubIdentityConfig } from '../config';
let requestTokens = 30;
let lastRefill = performance.now();
let activeRequests = 0;
export const ENROLL_PATH = '/api/collector/enroll';

export interface EnrollmentService { enroll(input: {collectorId: string; token: string; requestId?: string; recoverySecret?: string}): HubIdentityConfig }
/** Small bounded endpoint; grants authorize this mutation, browsers cannot call it. */
export async function handleEnrollment(req: IncomingMessage, res: ServerResponse, store: EnrollmentService | undefined, registered: (identity: HubIdentityConfig) => void): Promise<void> {
  const reply = (code: number, value: object) => {
    res.writeHead(code, {'Content-Type':'application/json', 'Cache-Control':'no-store', 'Connection':'close'});
    res.end(JSON.stringify(value));
  };
  const now = performance.now();
  requestTokens = Math.min(30, requestTokens + Math.max(0, now - lastRefill) / 200);
  lastRefill = now;
  if (requestTokens < 1 || activeRequests >= 16) { reply(429, {error:'enrollment request budget exceeded'}); return; }
  requestTokens -= 1;
  if (req.method !== 'POST') { reply(405, {error:'POST required'}); return; }
  if (!store) { reply(404, {error:'enrollment disabled'}); return; }
  if (req.headers.origin !== undefined) { reply(403, {error:'origin not allowed'}); return; }
  const match = /^Bearer ([\x21-\x7e]{1,256})$/.exec(req.headers.authorization ?? '');
  if (!match) { reply(401, {error:'invalid enrollment grant'}); return; }
  if (req.headers['content-type'] !== 'application/json') { reply(415, {error:'JSON required'}); return; }
  activeRequests += 1;
  const timeout = setTimeout(() => req.destroy(), 5000);
  try {
    let body = '';
    let bytes = 0;
    for await (const chunk of req) {
      bytes += Buffer.byteLength(chunk);
      if (bytes > 1024) { reply(413, {error:'body too large'}); return; }
      body += chunk.toString();
    }
    let collectorId: string;
    let recovery: { requestId?: string; recoverySecret?: string } = {};
    try {
      const value = JSON.parse(body) as {collectorId?: unknown; requestId?: unknown; recoverySecret?: unknown};
      if (!value || typeof value !== 'object' || Object.keys(value).some(k => !['collectorId','requestId','recoverySecret'].includes(k)) || typeof value.collectorId !== 'string' || !/^[\x21-\x7e]{1,128}$/.test(value.collectorId)) throw new Error();
      collectorId = value.collectorId;
      if (value.requestId !== undefined || value.recoverySecret !== undefined) {
        if (typeof value.requestId !== 'string' || !/^[\x21-\x7e]{1,128}$/.test(value.requestId) ||
          typeof value.recoverySecret !== 'string' || !/^[A-Za-z0-9_-]{32,128}$/.test(value.recoverySecret)) throw new Error();
        recovery = { requestId: value.requestId, recoverySecret: value.recoverySecret };
      }
    } catch { reply(400, {error:'invalid enrollment request'}); return; }
    let identity: HubIdentityConfig;
    try { identity = store.enroll({collectorId, token:match[1], ...recovery}); }
    catch (error) {
      const code = (error as {code?: string})?.code;
      const status = code === 'persist-failed' || code === 'invalid-state' || code === 'invalid-config' ? 503 : 403;
      reply(status, {error:status === 503 ? 'enrollment storage unavailable' : 'enrollment denied'}); return;
    }
    registered(identity);
    reply(201, identity);
  } finally { clearTimeout(timeout); activeRequests -= 1; }
}
