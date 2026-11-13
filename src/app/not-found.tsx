import type { Metadata } from "next";
import Link from "next/link";

import { BrandMark } from "@/components/brand-mark";

export const metadata: Metadata = {
  title: "Not found",
};

/**
 * Static not-found document served by the hub for unknown UI paths. It is
 * intentionally standalone (no live provider data): the path may be mistyped
 * or a stale link, so the page only offers a way back.
 */
export default function NotFound() {
  return (
    <main className="flex min-h-dvh items-center justify-center bg-background p-6">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex items-center gap-2.5">
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md border border-border text-accent">
            <BrandMark className="h-4 w-4" aria-hidden />
          </span>
          <span className="text-sm font-semibold tracking-tight">Rigscope</span>
        </div>
        <div className="rounded-xl border border-border bg-card p-5">
          <h1 className="text-lg font-semibold tracking-tight">Page not found</h1>
          <p className="mt-2 text-sm text-muted-foreground">
            This address does not match a page in this hub.
          </p>
          <Link
            href="/"
            className="mt-4 inline-flex rounded-md border border-border px-3 py-1.5 text-sm text-foreground transition-colors hover:bg-card-strong"
          >
            Back to fleet
          </Link>
        </div>
      </div>
    </main>
  );
}
