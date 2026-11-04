import type { Metadata } from "next";
import { Suspense } from "react";

import { AppShell } from "@/components/shell/app-shell";
import { Panel } from "@/components/panel";
import { ProcessesView } from "@/components/processes/processes-view";

export const metadata: Metadata = {
  title: "Processes",
};

function ProcessesFallback() {
  return (
    <AppShell title="Processes">
      <Panel title="Process detail" className="mt-2">
        <p role="status" className="py-10 text-center text-sm text-muted-foreground">
          Loading the process view…
        </p>
      </Panel>
    </AppShell>
  );
}

export default function ProcessesPage() {
  return (
    <Suspense fallback={<ProcessesFallback />}>
      <ProcessesView />
    </Suspense>
  );
}
