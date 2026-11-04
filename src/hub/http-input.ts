import type { IncomingMessage } from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import type { BrowserSession } from './auth/sessions';
import { isTrustedOrigin, type AuthOriginRules } from './auth/http';

export class HttpInputError extends Error {
  constructor(readonly status: number, message: string) { super(message); }
}
export function requireMutation(req: IncomingMessage, origin: AuthOriginRules, session: BrowserSession): void {
  const csrf = req.headers['x-csrf-token'];
  if (!isTrustedOrigin(req, origin) || typeof csrf !== 'string' ||
    Buffer.byteLength(csrf) !== Buffer.byteLength(session.csrfToken) ||
    !timingSafeEqual(Buffer.from(csrf), Buffer.from(session.csrfToken))) throw new HttpInputError(403, 'origin or csrf token not allowed');
}
export async function readJsonBody(req: IncomingMessage, maxBytes = 8192): Promise<Record<string, unknown>> {
  if (!req.headers['content-type']?.startsWith('application/json')) throw new HttpInputError(415, 'JSON required');
  const timeout = setTimeout(() => req.destroy(), 5000); timeout.unref();
  try {
    let bytes = 0; const chunks: Buffer[] = [];
    for await (const chunk of req) {
      bytes += Buffer.byteLength(chunk);
      if (bytes > maxBytes) throw new HttpInputError(413, 'request body too large');
      chunks.push(Buffer.from(chunk));
    }
    let value: unknown;
    try { value = JSON.parse(Buffer.concat(chunks).toString('utf8')); }
    catch { throw new HttpInputError(400, 'invalid JSON'); }
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new HttpInputError(400, 'JSON object required');
    return value as Record<string, unknown>;
  } finally { clearTimeout(timeout); }
}
