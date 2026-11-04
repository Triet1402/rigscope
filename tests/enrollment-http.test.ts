import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import { handleEnrollment } from '../src/hub/enrollment/http';

test('enrollment HTTP bounds input, rejects browser access, and activates only persisted identities', async t => {
  const accepted: string[] = [];
  const server = createServer((req,res) => void handleEnrollment(req,res, {enroll: ({collectorId,token}) => {
    if (token !== 'test-grant') throw new Error('denied');
    return {collectorId, machineId:'local-machine', token:'new-independent-credential'};
  }}, identity => accepted.push(identity.collectorId)));
  server.listen(0,'127.0.0.1'); await new Promise<void>(r => server.once('listening',r));
  t.after(() => {server.closeAllConnections(); server.close();});
  const address = server.address(); assert.ok(address && typeof address === 'object');
  const url = `http://127.0.0.1:${address.port}/api/collector/enroll`;
  for (const [headers,body,status] of [
    [{}, '{}',401],
    [{Authorization:'Bearer test-grant', Origin:'http://elsewhere'}, '{}',403],
    [{Authorization:'Bearer test-grant'}, '{"collectorId":"c"}',201],
    [{Authorization:'Bearer wrong'}, '{"collectorId":"c"}',403],
    [{Authorization:'Bearer test-grant'}, '{"collectorId":"c","machineId":"spoof"}',400],
    [{Authorization:'Bearer test-grant'}, 'x'.repeat(1025),413],
  ] as const) {
    const response = await fetch(url,{method:'POST',headers:{'Content-Type':'application/json',...headers},body});
    assert.equal(response.status,status); await response.arrayBuffer();
  }
  assert.deepEqual(accepted,['c']);
});
