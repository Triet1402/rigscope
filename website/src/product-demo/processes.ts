import {useEffect, useState} from 'react';
import type {UseProcessDetailResult} from '../../../src/components/processes/use-process-detail';
import {makeProcesses} from './fixtures';
import {useLive} from './live';

type ProductHook = typeof import('../../../src/components/processes/use-process-detail').useProcessDetail;
export function useProcessDetail({machineId, paused}:Parameters<ProductHook>[0]):UseProcessDetailResult {
  const {lastReceivedLocalMs} = useLive();
  const [nonce, setNonce] = useState(0);
  const [snapshot, setSnapshot] = useState(() => ({machineId, now:Date.now(), detail:makeProcesses(machineId ?? 'workstation', Date.now())}));
  useEffect(() => {
    if (paused && snapshot.machineId === machineId) return;
    const now = lastReceivedLocalMs ?? Date.now();
    setSnapshot({machineId, now, detail:makeProcesses(machineId ?? 'workstation', now)});
  }, [lastReceivedLocalMs, machineId, paused, nonce]);
  return {state:{phase:paused ? 'paused' : 'ready', detail:snapshot.detail, message:null,
    intervalMs:5000, leaseMs:15000, lastReceivedMs:snapshot.now}, hidden:false, refresh:() => setNonce(n => n + 1)};
}
