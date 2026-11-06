import {useMemo} from 'react';
import type {HistoryError} from '../../../src/components/history/use-stored-history';
import {makeHistory} from './fixtures';
import {useLive} from './live';

type ProductHook = typeof import('../../../src/components/history/use-stored-history').useStoredHistory;
export function useStoredHistory({machineId, metricsKey, coreIdsKey, range, enabled = true, autoRefresh = true, refreshNonce = 0}:Parameters<ProductHook>[0]):ReturnType<ProductHook> {
  const {lastReceivedLocalMs} = useLive();
  // Freeze the query when the actual History controls pause refresh.
  const clock = autoRefresh ? Math.floor((lastReceivedLocalMs ?? Date.now()) / 5000) * 5000 : 0;
  const data = useMemo(() => enabled && machineId && metricsKey ? {
    key:`${machineId}|${metricsKey}|${range}|${coreIdsKey ?? 'all'}`,
    range, loadedAt:Date.now(), result:makeHistory(machineId, metricsKey, range, clock || Date.now(), coreIdsKey),
  } : null, [machineId, metricsKey, coreIdsKey, range, enabled, clock, refreshNonce]);
  return {data, error:null as HistoryError | null, loading:false, manualRefreshing:false};
}
