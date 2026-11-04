import { ConfigError, validateHubUrl, parseCollectorConfig } from './config';
import { updateInstallation } from './local-state';
import { randomUUID, randomBytes } from 'node:crypto';

/** Explicit local administrative operation. Re-run separately for every hub. */
export async function enrollCollector(path: string, connectUrl: string, enrollmentToken: string, sharing: {cpu: boolean; memory: boolean}, replace = false): Promise<void> {
  const issues: string[] = [];
  const url = validateHubUrl(connectUrl, 'enrollment destination', issues);
  if (!url || issues.length) throw new Error('invalid enrollment destination');
  if (!/^[\x21-\x7e]{1,256}$/.test(enrollmentToken)) throw new Error('invalid enrollment token');
  const state = updateInstallation(path);
  if ((!replace && state.destinations.some(d => d.url === url.href)) ||
    (!state.destinations.some(d => d.url === url.href) && state.destinations.length >= 8)) throw new Error('destination already enrolled or destination budget exhausted');
  // Save proof BEFORE the first request; a lost success response can then be
  // retried without letting a grant holder recover from collectorId alone.
  const prepared = updateInstallation(path, current => {
    if (current.pendingEnrollments?.some(p => p.url === url.href)) return current;
    return { ...current, pendingEnrollments: [...(current.pendingEnrollments ?? []),
      { url: url.href, requestId: randomUUID(), recoverySecret: randomBytes(32).toString('base64url') }] };
  });
  const proof = prepared.pendingEnrollments!.find(p => p.url === url.href)!;
  const endpoint = new URL(url.href);
  endpoint.protocol = url.protocol === 'wss:' ? 'https:' : 'http:';
  endpoint.pathname = '/api/collector/enroll';
  let response: Response;
  try {
    response = await fetch(endpoint, {
      method: 'POST', redirect: 'error', signal: AbortSignal.timeout(5000),
      headers: {'Authorization': `Bearer ${enrollmentToken}`, 'Content-Type': 'application/json'},
      body: JSON.stringify({collectorId: state.collectorId, requestId: proof.requestId, recoverySecret: proof.recoverySecret}),
    });
  } catch { throw new ConfigError(['enrollment request failed; check destination and TLS trust']); }
  if (response.status !== 201) { await response.body?.cancel(); throw new ConfigError([`enrollment rejected (${response.status}); check the grant and hub enrollment state`]); }
  // A credential response must fit the small bounded contract.
  const reader = response.body?.getReader();
  if (!reader) throw new Error('missing enrollment response');
  let text = '';
  let bytes = 0;
  try {
    for (;;) {
      const {value, done} = await reader.read();
      if (done) break;
      bytes += value.byteLength;
      if (bytes > 4096) throw new Error('oversized enrollment response');
      text += new TextDecoder().decode(value, {stream: true});
    }
  } finally { await reader.cancel(); }
  const result = JSON.parse(text) as {collectorId?: unknown; machineId?: unknown; token?: unknown};
  if (result.collectorId !== state.collectorId || typeof result.machineId !== 'string' || !result.machineId || typeof result.token !== 'string') throw new Error('invalid enrollment response');
  const destination = parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url: url.href, token: result.token, sharing}])}).destinations[0];
  try { updateInstallation(path, current => {
    if (current.collectorId !== state.collectorId || (!replace && current.destinations.some(d => d.url === url.href)) ||
      (!current.destinations.some(d => d.url === url.href) && current.destinations.length >= 8)) throw new Error('collector state changed during enrollment');
    return {...current, destinations: [...current.destinations.filter(d => d.url !== url.href), destination],
      pendingEnrollments: (current.pendingEnrollments ?? []).filter(p => p.url !== url.href)};
  }); } catch {
    throw new ConfigError(['hub enrollment succeeded but local credential save failed; operator recovery is required']);
  }
}
