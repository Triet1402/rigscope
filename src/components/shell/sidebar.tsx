"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Bell, Cable, History, LayoutDashboard, List, Server, Settings } from "lucide-react";

import { machineViewHref, useMachineNavigation, type MachineViewKind } from "@/live/machine-selection";
import { BrandMark } from "@/components/brand-mark";
import { SidebarLiveStatus } from "@/components/live/live-status";
import { useLive } from "@/live/live-provider";
import { stateFreshness } from "@/live/presentation";
import { useNow } from "@/live/use-now";

const GLOBAL_LINKS = [
  { href: "/alerts", label: "Alerts", icon: Bell },
  { href: "/connections", label: "Connections", icon: Cable },
] as const;
const MACHINE_LINKS = [
  { view: "overview", label: "Overview", icon: LayoutDashboard },
  { view: "history", label: "History", icon: History },
  { view: "processes", label: "Processes", icon: List },
] as const;

const linkStyle = (active: boolean) => [
  "flex min-h-11 min-w-0 md:min-h-9 items-center gap-3 rounded-md border px-2 text-sm transition-colors md:px-3",
  active ? "border-accent/60 bg-selected font-medium text-foreground" : "border-transparent text-muted-foreground hover:bg-card-strong hover:text-foreground",
].join(" ");

export function Sidebar({ machineId, machineView, expanded = false }: { machineId?: string; machineView?: MachineViewKind; expanded?: boolean }) {
  const pathname = usePathname();
  const { session, machines, phase } = useLive();
  const now = useNow();
  const { machine: selected, view: activeView, selectMachine } = useMachineNavigation(machineId, machineView);
  const admin = session.kind === "signed-in" && session.user.admin === true;
  const globalLinks = admin ? [...GLOBAL_LINKS, { href: "/administration", label: "Administration", icon: Settings }] : GLOBAL_LINKS;

  return (
    <aside className={`flex shrink-0 flex-col border-r border-border bg-background ${expanded ? "w-full" : "w-56"}`}>
      <Link href="/" aria-label="Rigscope" className="flex h-14 shrink-0 items-center gap-2.5 border-b border-border px-3 md:px-5">
        <BrandMark className="h-5 w-5 shrink-0 text-accent" aria-hidden />
        <span className="hidden text-sm font-semibold tracking-tight text-foreground md:inline">Rigscope</span>
      </Link>
      <nav aria-label="Primary" className="flex min-h-0 flex-1 flex-col overflow-y-auto p-2 md:p-3">
        <Link href="/" aria-current={pathname === "/" ? "page" : undefined} title="Fleet" aria-label="Fleet" className={linkStyle(pathname === "/")}>
          <Server className="h-4 w-4 shrink-0" aria-hidden /><span className="inline">Fleet</span>
        </Link>
        {machines.length > 0 ? (
          <div role="group" aria-label="Select a machine" className="my-3 flex max-h-[min(30dvh,20rem)] shrink-0 flex-col gap-1 overflow-y-auto">
            {machines.map((machine) => {
              const name = machine.displayName || machine.hostname || machine.machineId;
              const tags = machine.tags?.join(", ");
              const isSelected = selected?.machineId === machine.machineId;
              const fresh = stateFreshness(machine, now);
              const status = !machine.connected ? "Disconnected"
                : phase !== "live" ? "Feed unavailable"
                : fresh === "stale" ? "Stale"
                : fresh === "unknown" ? "No sample yet" : "Live";
              const dotStyle = status === "Live" ? "bg-ok"
                : status === "Disconnected" ? "bg-crit"
                : status === "No sample yet" ? "bg-muted-foreground" : "bg-warn";
              const description = machine.inventory?.cpu.model || tags;
              return (
                <button key={machine.machineId} type="button" aria-pressed={isSelected}
                  aria-label={`${name}: ${status}${description ? `, ${description}` : ""}`}
                  title={[name, status, description, tags].filter(Boolean).join(" · ")}
                  onClick={() => selectMachine(machine.machineId)}
                  className={`${linkStyle(isSelected)} w-full shrink-0 py-2 text-left focus-visible:outline-offset-[-2px] `}>
                  <span aria-hidden className="relative flex h-5 w-4 shrink-0 items-center justify-center">
                    <span className="hidden">{name.slice(0, 2).toUpperCase()}</span>
                    <span className={`h-2 w-2 shrink-0 rounded-full ${dotStyle}`} />
                  </span>
                  <span className="min-w-0 flex-1 block">
                    <span className="block break-words font-medium leading-5">{name}</span>
                    {description ? <span className={`mt-0.5 block break-words text-xs leading-4 ${isSelected ? "text-foreground/80" : "text-muted-foreground"}`}>{description}</span> : null}
                  </span>
                </button>
              );
            })}
          </div>
        ) : null}
        {selected ? (
          <div role="group" aria-label="Selected machine views" className="mb-3 flex shrink-0 flex-col gap-1 border-t border-border pt-3">
            {MACHINE_LINKS.map(({ view, label, icon: Icon }) => (
              <Link key={view} href={machineViewHref(selected.machineId, view)} aria-current={activeView === view ? "page" : undefined}
                aria-label={`${selected.displayName || selected.hostname || selected.machineId}: ${label}`}
                title={`${selected.displayName || selected.hostname || selected.machineId}: ${label}`} className={linkStyle(activeView === view)}>
                <Icon className="h-4 w-4 shrink-0" aria-hidden /><span className="inline">{label}</span>
              </Link>
            ))}
          </div>
        ) : null}
        <div className="flex shrink-0 flex-col gap-1 border-t border-border pt-3">
          {globalLinks.map(({ href, label, icon: Icon }) => (
            <Link key={href} href={href} aria-current={pathname === href ? "page" : undefined} title={label} aria-label={label} className={linkStyle(pathname === href)}>
              <Icon className="h-4 w-4 shrink-0" aria-hidden /><span className="inline">{label}</span>
            </Link>
          ))}
        </div>
      </nav>
      <footer className="shrink-0 border-t border-border p-3"><SidebarLiveStatus /></footer>
    </aside>
  );
}
