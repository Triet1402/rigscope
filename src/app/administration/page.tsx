import type { Metadata } from "next";

import { AdministrationView } from "@/components/admin/administration-view";

export const metadata: Metadata = {
  title: "Administration",
};

export default function AdministrationPage() {
  return <AdministrationView />;
}
