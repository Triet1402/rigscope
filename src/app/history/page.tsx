import type { Metadata } from "next";
import { Suspense } from "react";

import { AppShell } from "@/components/shell/app-shell";
import { Panel } from "@/components/panel";
import { HistoryView } from "@/components/history/history-view";

export const metadata: Metadata = {
  title: "History",
};

function HistoryFallback() {
  return (
    <AppShell title="History">
      <div className="grid grid-cols-1 gap-5 xl:grid-cols-2" aria-busy="true">
        <Panel title="Loading history">
          <div className="h-40 animate-pulse rounded-lg bg-card-strong" aria-hidden />
          <p className="mt-3 text-xs text-muted-foreground">Preparing the history view…</p>
        </Panel>
        <Panel title="Preparing charts">
          <div className="h-40 animate-pulse rounded-lg bg-card-strong" aria-hidden />
          <p className="mt-3 text-xs text-muted-foreground">Machine and metric controls appear next.</p>
        </Panel>
      </div>
    </AppShell>
  );
}

export default function HistoryPage() {
  return (
    <Suspense fallback={<HistoryFallback />}>
      <HistoryView />
    </Suspense>
  );
}
