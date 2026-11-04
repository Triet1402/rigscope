import type { Metadata } from "next";

import { ConnectionsView } from "@/components/resources/connections-panels";

export const metadata: Metadata = {
  title: "Connections",
};

export default function ConnectionsPage() {
  return <ConnectionsView />;
}
