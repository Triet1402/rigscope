import type { LucideIcon } from "lucide-react";

/**
 * A data panel: card surface, hairline border, panel title, optional
 * action slot on the right. The one container vocabulary used across
 * Overview / History / Processes.
 */
export function Panel({
  title,
  action,
  className,
  children,
}: {
  title: string;
  action?: React.ReactNode;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <section
      className={[
        "flex min-w-0 flex-col rounded-xl border border-border bg-card p-5",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      <div className="flex shrink-0 items-center justify-between gap-4">
        <h2 className="text-sm font-medium tracking-tight">{title}</h2>
        {action ? <div className="text-xs tabular-nums">{action}</div> : null}
      </div>
      <div className="mt-4 flex min-h-0 flex-1 flex-col">{children}</div>
    </section>
  );
}

/**
 * Empty state that teaches the interface: what lives here, and what
 * arrives when it does. Renders inside a Panel's content area.
 */
export function EmptyState({
  icon: Icon,
  note,
  className,
}: {
  icon: LucideIcon;
  note: string;
  className?: string;
}) {
  return (
    <div
      className={[
        "flex flex-1 flex-col items-center justify-center gap-2.5 rounded-lg border border-dashed border-border px-6 py-10 text-center",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      <Icon className="h-5 w-5 text-muted-foreground" aria-hidden />
      <p className="text-sm text-muted-foreground">{note}</p>
    </div>
  );
}
