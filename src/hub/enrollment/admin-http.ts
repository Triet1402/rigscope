import type { IncomingMessage, ServerResponse } from 'node:http';
import type { BrowserHttpAuth, AuthOriginRules } from '../auth/http';
import { sendJson } from '../auth/http';
import { HttpInputError, readJsonBody, requireMutation } from '../http-input';
import { CollectorRegistry, RegistryError } from './registry';

export class CollectorAdminHttp {
  constructor(private readonly options: { registry: CollectorRegistry; auth: BrowserHttpAuth;
    origin: AuthOriginRules; isAdmin: (username: string) => boolean; changed: () => void }) {}
  async handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<boolean> {
    if (!['/api/admin/collectors', '/api/admin/collector-grants'].includes(url.pathname)) return false;
    const { registry, auth, origin, isAdmin, changed } = this.options;
    const session = auth.requestSession(req);
    if (!session) { sendJson(res, 401, { error: 'authentication required' }); return true; }
    if (!isAdmin(session.username)) { sendJson(res, 403, { error: 'administrative access required' }); return true; }
    const allowed = (machineId: string) => session.scope === '*' || session.scope.includes(machineId);
    try {
      if (url.pathname === '/api/admin/collectors' && req.method === 'GET') {
        sendJson(res, 200, { collectors: registry.list().filter(row => allowed(row.machineId)) }); return true;
      }
      if (req.method !== 'POST') { sendJson(res, 405, { error: 'POST required' }); return true; }
      requireMutation(req, origin, session);
      const body = await readJsonBody(req, 2048);
      if (url.pathname === '/api/admin/collector-grants') {
        if (Object.keys(body).some(k => !['replaceMachineId', 'ttlMs'].includes(k)) ||
          (body.replaceMachineId !== undefined && typeof body.replaceMachineId !== 'string') ||
          (body.ttlMs !== undefined && typeof body.ttlMs !== 'number')) throw new HttpInputError(400, 'invalid grant request');
        const replacement = body.replaceMachineId as string | undefined;
        if (replacement ? !allowed(replacement) : session.scope !== '*') throw new HttpInputError(403, 'machine access denied');
        // Only a short-lived, single-use bootstrap grant is revealed explicitly.
        // Installed collector bearer credentials never enter a browser response.
        sendJson(res, 201, registry.createGrant(session.username, { replaceMachineId: replacement, ttlMs: body.ttlMs as number | undefined }));
      } else {
        if (Object.keys(body).sort().join(',') !== 'action,collectorId' || body.action !== 'revoke' || typeof body.collectorId !== 'string')
          throw new HttpInputError(400, 'explicit collector revocation required');
        const collector = registry.list().find(row => row.collectorId === body.collectorId);
        if (!collector || !allowed(collector.machineId)) throw new HttpInputError(403, 'machine access denied');
        registry.revoke(session.username, collector.collectorId); changed(); sendJson(res, 200, { ok: true });
      }
    } catch (error) {
      if (error instanceof HttpInputError) sendJson(res, error.status, { error: error.message });
      else if (error instanceof RegistryError) sendJson(res, error.code === 'cap-reached' ? 409 : 400, { error: error.message });
      else sendJson(res, 503, { error: 'collector administration temporarily unavailable' });
    }
    return true;
  }
}
