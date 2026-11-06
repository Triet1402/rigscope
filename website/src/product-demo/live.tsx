import {createContext, useContext, useEffect, useMemo, useState, type ReactNode} from 'react';
import type {LiveContextValue, SampleHistoryPoint} from '../../../src/live/types';
import {makeMachines} from './fixtures';

const Context = createContext<LiveContextValue | null>(null);
export function useLive():LiveContextValue {
  const value = useContext(Context);
  if (!value) throw new Error('Missing synthetic demo provider');
  return value;
}

export function DemoLiveProvider({children}:{children:ReactNode}) {
  const [now, setNow] = useState(Date.now);
  const [notice, setNotice] = useState<string | null>(null);
  useEffect(() => {
    const timer = window.setInterval(() => { if (!document.hidden) setNow(Date.now()); }, 5000);
    return () => window.clearInterval(timer);
  }, []);
  const value = useMemo<LiveContextValue>(() => {
    const machines = makeMachines(now);
    const history = new Map<string, SampleHistoryPoint[]>();
    for (const machine of machines) history.set(machine.machineId, []);
    for (let i = 0; i <= 60; i++) {
      const t = now - (60 - i) * 5000;
      for (const machine of makeMachines(t)) {
        const payload = machine.metrics!.payload;
        history.get(machine.machineId)!.push({sampleId:String(i), receivedAtLocal:t,
          cpuPercent:payload.cpu.status === 'ok' ? payload.cpu.usagePercent : null,
          memoryPercent:payload.memory.status === 'ok' ? payload.memory.usedBytes / payload.memory.totalBytes * 100 : null,
          gapThresholdMs:15000});
      }
    }
    return {session:{kind:'signed-in', user:{username:'demo', csrfToken:'synthetic-only', admin:true}},
      phase:'live', hubRunId:'synthetic-demo', machines, lastReceivedLocalMs:now,
      lastEventId:String(now), lastEventKind:'update', notice, history,
      login:async () => ({kind:'error', detail:'Sign-in is unavailable in the synthetic demo.'}),
      signOut:async () => setNotice('This is a public demo with synthetic data. No account is signed in.'),
      retryConnection:() => setNow(Date.now()), dismissNotice:() => setNotice(null)};
  }, [now, notice]);
  return <Context.Provider value={value}>{children}</Context.Provider>;
}
