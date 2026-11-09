/** Authenticated monitoring queries and ephemeral process interests. */
import { MAX_CORES } from '../contracts';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import type { BrowserHttpAuth, AuthOriginRules } from './auth/http';
import { isTrustedOrigin, sendJson } from './auth/http';
import type { HubState } from './state';
import type { HistorySource } from './storage/history';
import { HistoryQueryError } from './storage/history-query';
import { METRICS } from './storage/series';
import type { TelemetryStore } from './storage/telemetry-store';
import type { MemoryTelemetryStore } from './storage/memory-store';
import { ProcessViewerBudgetError, type ProcessInterests } from './process-interests';
import { HttpInputError, readJsonBody } from './http-input';

export type MonitorStorage = HistorySource & Pick<
  TelemetryStore | MemoryTelemetryStore,
  'getMachineMeta' | 'setMachineMeta'
>;

export class MonitorHttp {
  private readonly historyBudgets = new Map<string, { at: number; tokens: number }>();
  constructor(private readonly options: { auth: BrowserHttpAuth; origin: AuthOriginRules; state: HubState;
    database?: unknown; storage: MonitorStorage; processes: ProcessInterests;
    isAdmin?: (username: string) => boolean }) {}

  async handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<boolean> {
    const path = url.pathname;
    if (!['/api/history', '/api/history/metrics', '/api/history/status', '/api/processes', '/api/machines/meta'].includes(path)) return false;
    const { auth, origin, state, processes, storage } = this.options;
    const session = auth.requestSession(req);
    if (!session) { sendJson(res, 401, { error: 'authentication required' }); return true; }
    const method = req.method ?? 'GET';
    if (req.headers.origin !== undefined && !isTrustedOrigin(req, origin)) {
      sendJson(res, 403, { error: 'origin not allowed' }); return true;
    }
    if (!['GET', 'POST', 'DELETE'].includes(method)) {
      sendJson(res, 405, { error: 'method not allowed' }); return true;
    }
    if (method !== 'GET') {
      const csrf = req.headers['x-csrf-token'];
      if (!isTrustedOrigin(req, origin) || typeof csrf !== 'string' ||
        Buffer.byteLength(csrf) !== Buffer.byteLength(session.csrfToken) ||
        !timingSafeEqual(Buffer.from(csrf), Buffer.from(session.csrfToken))) {
        sendJson(res, 403, { error: 'missing or invalid csrf token' }); return true;
      }
    }
    if (path === '/api/history/status') {
      if (method !== 'GET') { sendJson(res, 405, { error: 'GET required' }); return true; }
      sendJson(res, 200, storage.historyStatus());
      return true;
    }
    const machineId = url.searchParams.get('machineId') ?? '';
    if (!machineId || machineId.length > 128 || (session.scope !== '*' && !session.scope.includes(machineId))) {
      sendJson(res, 403, { error: 'machine access denied' }); return true;
    }
    const machine = state.snapshot().machines.find(m => m.machineId === machineId);
    if (!machine) { sendJson(res, 404, { error: 'machine not found' }); return true; }
    try {
      if (path === '/api/history/metrics') {
        if (method !== 'GET') { sendJson(res, 405, { error: 'GET required' }); return true; }
        sendJson(res, 200, { metrics: METRICS });
      } else if (path === '/api/history') {
        if (method !== 'GET') { sendJson(res, 405, { error: 'GET required' }); return true; }
        // Bound repeated scans per session and globally through the session store cap.
        const now = performance.now();
        const budget = this.historyBudgets.get(session.id) ?? { at: now, tokens: 8 };
        // A session is shared by tabs; allow four visible pollers plus a
        // bounded selection burst without relaxing per-query scan limits.
        budget.tokens = Math.min(8, budget.tokens + (now - budget.at) / 250); budget.at = now;
        if (budget.tokens < 1) { sendJson(res, 429, { error: 'Too many history requests. Pause another History tab, then refresh.' }); return true; }
        budget.tokens--;
        if (this.historyBudgets.size >= 256 && !this.historyBudgets.has(session.id)) this.historyBudgets.delete(this.historyBudgets.keys().next().value!);
        this.historyBudgets.set(session.id, budget);
        const result = storage.queryHistory({ machineId,
          metrics: (url.searchParams.get('metrics') ?? 'cpu.usage,memory.used').split(','),
          ...(url.searchParams.has('resourceIds') ? { resourceIds: url.searchParams.get('resourceIds')!.split(',') } : {}),
          ...(url.searchParams.has('coreIds') ? { coreIds: url.searchParams.get('coreIds')!.split(',').filter(Boolean) } : {}),
          fromMs: Number(url.searchParams.get('from')), toMs: Number(url.searchParams.get('to')),
          maxPoints: Number(url.searchParams.get('maxPoints') ?? 600) }, { maxRowsScanned: 25000, maxElapsedMs: 100,
          maxSeries: url.searchParams.get("metrics")?.split(",").includes("cpu.core") ? MAX_CORES + 64 : 64 });
        sendJson(res, 200, result);
      } else if (path === '/api/processes') {
        const viewerId = url.searchParams.get('viewerId') ?? '';
        if (!/^[a-zA-Z0-9-]{1,128}$/.test(viewerId)) { sendJson(res, 400, { error: 'invalid viewer identity' }); return true; }
        if (method === 'DELETE') { processes.cancel(session.id, viewerId); sendJson(res, 200, { ok: true }); }
        else if (method === 'POST') {
          if (!machine.connected) { sendJson(res, 409, { error: 'collector not connected for Processes' }); return true; }
          const capability = machine.inventory?.capabilities.processes;
          if (capability && capability.status !== 'supported') { sendJson(res, 403, { error: capability.reason ?? `processes ${capability.status}` }); return true; }
          sendJson(res, 200, processes.renew(session.id, machineId, viewerId));
        } else {
          const result = processes.read(session.id, machineId, viewerId);
          sendJson(res, result ? 200 : 410, result ?? { error: 'process interest expired; renew to continue' });
        }
      } else {
        if (method !== 'POST') { sendJson(res, 405, { error: 'POST required' }); return true; }
        if (!this.options.isAdmin?.(session.username)) { sendJson(res, 403, { error: 'administrative access required' }); return true; }
        const body = await readJsonBody(req, 4096);
        if (Object.keys(body).some(k => !['displayName', 'tags'].includes(k)) || Object.keys(body).length === 0)
          throw new HttpInputError(400, 'invalid machine metadata');
        if (body.displayName !== undefined && body.displayName !== null &&
          (typeof body.displayName !== 'string' || body.displayName.length > 128 || /[\u0000-\u001f\u007f]/.test(body.displayName)))
          throw new HttpInputError(400, 'invalid display name');
        if (body.tags !== undefined && (!Array.isArray(body.tags) || body.tags.length > 32 ||
          body.tags.some(t => typeof t !== 'string' || !t.trim() || t.length > 64 || /[\u0000-\u001f\u007f]/.test(t))))
          throw new HttpInputError(400, 'invalid machine tags');
        const existing = storage.getMachineMeta(machineId);
        const meta = { displayName: body.displayName === undefined ? existing.displayName : body.displayName as string | null,
          tags: body.tags === undefined ? existing.tags : [...new Set((body.tags as string[]).map(t => t.trim()))] };
        storage.setMachineMeta(machineId, meta); state.setMetadata(machineId, meta);
        sendJson(res, 200, meta);
      }
    } catch (error) {
      if (error instanceof HttpInputError) sendJson(res, error.status, { error: error.message });
      else if (error instanceof ProcessViewerBudgetError) sendJson(res, 429, { error: error.message, code: 'process-viewer-budget' });
      else if (error instanceof HistoryQueryError) sendJson(res, 400, { error: error.message });
      else sendJson(res, 503, { error: 'monitoring request temporarily unavailable' });
    }
    return true;
  }
}
