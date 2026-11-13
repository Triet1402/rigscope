import { Suspense } from "react";
import type { Metadata } from "next";

import { MachinePageFallback, MachinePageView } from "./machine-page";

export const metadata: Metadata = {
  title: "Machine",
};

/**
 * The machine overview is the exported page `out/machines.html`, addressed as
 * `/machines?machineId=<id>`. The legacy `/machines/<id>` URL is redirected
 * to that form by the hub static handler in `internal/hub/webui`, so every
 * machine link is a normal client-side navigation rather than a full document
 * load.
 */
export default function MachinesPage() {
  // The selection is read from the query string, which is only available on
  // the client, so the static prerender needs a Suspense boundary.
  return (
    <Suspense fallback={<MachinePageFallback />}>
      <MachinePageView />
    </Suspense>
  );
}
