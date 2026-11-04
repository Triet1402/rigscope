import { MachineBreadcrumb } from "@/components/live/machine-navigation";
import type { MachineViewKind } from "@/live/machine-selection";
import { Sidebar } from "./sidebar";
import { ThemeToggle } from "@/components/theme-toggle";

/**
 * The fixed frame: sidebar, header, scrolling main region. `headerRight`
 * lets live views place status/auth controls beside the theme toggle;
 * the title doubles as the page's unique h1.
 */
export function AppShell({
  title,
  headerRight,
  children,
  workspace = false,
  machineId,
  machineView,
}: {
  title: string;
  machineId?: string;
  machineView?: MachineViewKind;
  workspace?: boolean;
  headerRight?: React.ReactNode;
  children: React.ReactNode;
}) {
  /*
   * Every page shares one content container so page width and centering
   * match across the app: capped, centered, and full-height only in the
   * workspace mode where the page fills the viewport vertically.
   */
  const pageContainer = workspace
    ? "mx-auto flex h-full w-full min-h-0 min-w-0 max-w-[1500px] flex-1 flex-col"
    : "mx-auto flex w-full min-w-0 max-w-[1500px] flex-col";

  return (
    <div className="relative flex h-dvh overflow-hidden bg-background text-foreground">
      <a href="#main-content" className="sr-only z-50 rounded-md bg-card px-4 py-2 text-foreground focus:not-sr-only focus:absolute focus:start-2 focus:top-2 focus:outline-2 focus:outline-accent">
        Skip to content
      </a>
      <Sidebar machineId={machineId} machineView={machineView} />
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex min-h-14 shrink-0 flex-wrap items-center justify-between gap-2 border-b border-border px-4 py-2 md:px-6">
          {machineId && machineView ? <MachineBreadcrumb machineId={machineId} title={title} view={machineView} /> : <h1 className="truncate text-lg font-semibold tracking-tight">{title}</h1>}
          <div className="flex items-center gap-3">
            {headerRight ? <div className="flex min-w-0 items-center gap-3">{headerRight}</div> : null}
            <ThemeToggle />
          </div>
        </header>
        {/* relative: the containing block for sr-only/absolute descendants, so they stay clipped here. */}
        <main id="main-content" tabIndex={-1} className={`relative min-h-0 min-w-0 flex-1 overflow-x-hidden overscroll-contain p-4 outline-offset-[-2px] md:p-6 ${workspace ? "flex flex-col overflow-y-hidden" : "overflow-y-auto"}`}>
          <div className={pageContainer}>{children}</div>
        </main>
      </div>
    </div>
  );
}
