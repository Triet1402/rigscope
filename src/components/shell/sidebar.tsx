"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Bell, Cable, History, LayoutDashboard, List, Server, Settings } from "lucide-react";

import { machineViewHref, useMachineNavigation, type MachineViewKind } from "@/live/machine-selection";
import { BrandMark } from "@/components/brand-mark";
import { SidebarLiveStatus } from "@/components/live/live-status";
import { useLive } from "@/live/live-provider";

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
  "flex min-h-9 min-w-0 items-center gap-3 rounded-md px-2 text-sm transition-colors md:px-3",
  active ? "bg-selected font-medium text-foreground" : "text-muted-foreground hover:bg-card-strong hover:text-foreground",
].join(" ");

export function Sidebar({ machineId, machineView }: { machineId?: string; machineView?: MachineViewKind }) {
  const pathname = usePathname();
  const { session, machines } = useLive();
  const { machine: selected, view: activeView, selectMachine } = useMachineNavigation(machineId, machineView);
  const admin = session.kind === "signed-in" && session.user.admin === true;
  const globalLinks = admin ? [...GLOBAL_LINKS, { href: "/administration", label: "Administration", icon: Settings }] : GLOBAL_LINKS;

  return (
    <aside className="flex w-14 shrink-0 flex-col border-r border-border bg-card md:w-56">
      <Link href="/" aria-label="Rigscope" className="flex h-14 shrink-0 items-center gap-2.5 border-b border-border px-3 md:px-5">
        <BrandMark className="h-5 w-5 shrink-0 text-accent" aria-hidden />
        <span className="hidden text-sm font-semibold tracking-tight text-foreground md:inline">Rigscope</span>
      </Link>
      <nav aria-label="Primary" className="flex min-h-0 flex-1 flex-col overflow-y-auto p-2 md:p-3">
        <Link href="/" aria-current={pathname === "/" ? "page" : undefined} title="Fleet" className={linkStyle(pathname === "/")}>
          <Server className="h-4 w-4 shrink-0" aria-hidden /><span className="hidden md:inline">Fleet</span>
        </Link>
        {selected ? (
          <div className="mb-4 mt-2 md:ms-3 md:border-s md:border-border md:ps-3">
            <label htmlFor="sidebar-machine" className="sr-only">Machine</label>
            <select id="sidebar-machine" value={selected.machineId} onChange={(event) => selectMachine(event.target.value)}
              className="mb-1 hidden h-9 w-full min-w-0 rounded-md border border-border bg-background px-2 text-sm font-medium md:block">
              {machines.map((machine) => <option key={machine.machineId} value={machine.machineId}>{machine.displayName || machine.hostname || machine.machineId}</option>)}
            </select>
            <nav aria-label="Selected machine" className="flex flex-col gap-1">
              {MACHINE_LINKS.map(({ view, label, icon: Icon }) => (
                <Link key={view} href={machineViewHref(selected.machineId, view)} aria-current={activeView === view ? "page" : undefined}
                  title={`${selected.displayName || selected.hostname}: ${label}`} className={linkStyle(activeView === view)}>
                  <Icon className="h-4 w-4 shrink-0" aria-hidden /><span className="hidden md:inline">{label}</span>
                </Link>
              ))}
            </nav>
          </div>
        ) : null}
        <div className="flex flex-col gap-1 border-t border-border pt-3">
          {globalLinks.map(({ href, label, icon: Icon }) => (
            <Link key={href} href={href} aria-current={pathname === href ? "page" : undefined} title={label} className={linkStyle(pathname === href)}>
              <Icon className="h-4 w-4 shrink-0" aria-hidden /><span className="hidden md:inline">{label}</span>
            </Link>
          ))}
        </div>
      </nav>
      <footer className="hidden shrink-0 border-t border-border p-3 md:block"><SidebarLiveStatus /></footer>
    </aside>
  );
}
