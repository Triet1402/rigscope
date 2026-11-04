"use client";

/**
 * Auth controls for the page header: a sign-in link when signed out, and
 * the signed-in username plus a sign-out button when a session exists.
 * Sign-out uses the provider, which sends the CSRF header from the session
 * payload and stops the live stream per the backend agreement.
 */

import Link from "next/link";

import { useLive } from "@/live/live-provider";

export function UserMenu() {
  const { session, phase, signOut } = useLive();

  if (session.kind === "signed-in") {
    return (
      <div className="flex items-center gap-2">
        <span className="hidden max-w-[12rem] truncate text-xs text-muted-foreground sm:inline">
          {session.user.username}
        </span>
        <button
          type="button"
          onClick={() => void signOut()}
          className="rounded-md px-2.5 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-card-strong hover:text-foreground"
        >
          Sign out
        </button>
      </div>
    );
  }

  if (session.kind === "restoring" || session.kind === "auth-unavailable") {
    return null;
  }

  // Signed out, expired (phase), or restore-failed: offer sign-in and,
  // on restore failure, a retry (the stream may just be unreachable).
  return (
    <div className="flex items-center gap-1">
      <Link
        href="/login"
        className="rounded-md border border-border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
      >
        Sign in
      </Link>
    </div>
  );
}
