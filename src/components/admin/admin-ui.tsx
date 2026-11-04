import type { ReactNode } from "react";
import { AdminApiError, isAbortError } from "@/admin/api";

/** Shared control vocabulary for the administration surface (Operate mode). */
export const ADMIN_INPUT =
  "w-full min-w-0 rounded-md border border-border bg-background px-3 py-2 text-base text-foreground placeholder:text-muted-foreground aria-[invalid=true]:border-crit sm:text-sm";

export const ADMIN_BUTTON_PRIMARY =
  "inline-flex min-h-9 items-center justify-center gap-1.5 rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-accent-foreground transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50";

export const ADMIN_BUTTON_SECONDARY =
  "inline-flex min-h-9 items-center justify-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-card-strong disabled:cursor-not-allowed disabled:opacity-50";

export const ADMIN_BUTTON_QUIET =
  "inline-flex min-h-8 items-center justify-center gap-1.5 rounded-md px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-card-strong hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50";

export const ADMIN_BUTTON_DANGER =
  "inline-flex min-h-9 items-center justify-center gap-1.5 rounded-md border border-crit px-3 py-1.5 text-xs font-medium text-crit transition-colors hover:bg-crit-soft disabled:cursor-not-allowed disabled:opacity-50";

export function Field({
  id,
  label,
  error,
  children,
}: {
  id: string;
  label: string;
  error?: string;
  children: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <label htmlFor={id} className="text-xs font-medium text-foreground">
        {label}
      </label>
      {children}
      {error ? (
        <p id={`${id}-error`} className="text-xs text-crit">
          {error}
        </p>
      ) : null}
    </div>
  );
}

export function describedBy(id: string, error?: string): string | undefined {
  if (error) return `${id}-error`;
  return undefined;
}

export function Badge({
  children,
  tone = "neutral",
}: {
  children: ReactNode;
  tone?: "neutral" | "accent" | "ok" | "warn" | "crit";
}) {
  const classes: Record<string, string> = {
    neutral: "border border-border text-muted-foreground",
    accent: "bg-selected text-foreground",
    ok: "bg-ok-soft text-foreground",
    warn: "bg-warn-soft text-foreground",
    crit: "bg-crit-soft text-foreground",
  };
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium ${classes[tone]}`}
    >
      {children}
    </span>
  );
}

export function InlineNotice({
  children,
  tone = "info",
  role = "status",
}: {
  children: ReactNode;
  tone?: "info" | "error" | "success" | "warn";
  role?: "status" | "alert";
}) {
  const classes: Record<string, string> = {
    info: "text-muted-foreground",
    error: "bg-crit-soft text-foreground",
    success: "bg-ok-soft text-foreground",
    warn: "bg-warn-soft text-foreground",
  };
  return (
    <p
      role={role}
      className={`rounded-md px-3 py-2 text-xs ${classes[tone]}`}
    >
      {children}
    </p>
  );
}

export function friendlyError(error: unknown, fallback: string): string {
  if (error instanceof AdminApiError) return error.message;
  return fallback;
}

export function isUnauthorized(error: unknown): boolean {
  return error instanceof AdminApiError && error.status === 401;
}

export function isForbidden(error: unknown): boolean {
  return error instanceof AdminApiError && error.status === 403;
}

export function isAbort(error: unknown): boolean {
  return isAbortError(error);
}
