import { MachineView } from "@/components/live/machine-view";

export const metadata = {
  title: "Machine",
};

export default async function MachinePage({
  params,
}: {
  params: Promise<{ machineId: string }>;
}) {
  const { machineId } = await params;
  return <MachineView machineId={machineId} />;
}
