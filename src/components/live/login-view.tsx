"use client";

import { SourceLink } from "@/components/source-link";

/**
 * The /login surface. A small standalone card (no app shell) with a labeled
 * form. Outcomes follow the backend agreement exactly: 401 maps to a plain
 * "does not match" message, 503 explains that the hub administrator has to
 * configure browser users, and unreachable/network failures are distinct.
 */

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { AlertTriangle, ShieldAlert, WifiOff } from "lucide-react";

import { BrandMark } from "@/components/brand-mark";
import { useLive } from "@/live/live-provider";
import type { LoginOutcome } from "@/live/session";

function OutcomeMessage(props: { outcome: LoginOutcome }) {
  const { outcome } = props;
  switch (outcome.kind) {
    case "invalid-credentials":
      return (
        <p role="alert" className="flex items-center gap-2 text-xs text-warn">
          <AlertTriangle className="h-3.5 w-3.5 shrink-0" aria-hidden />
          That username or password does not match.
        </p>
      );
    case "auth-unavailable":
      return (
        <p role="alert" className="flex items-start gap-2 text-xs text-warn">
          <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          <span>
            Sign-in is unavailable. Contact your administrator.
          </span>
        </p>
      );
    case "unreachable":
      return (
        <p role="alert" className="flex items-center gap-2 text-xs text-crit">
          <WifiOff className="h-3.5 w-3.5 shrink-0" aria-hidden />
          Cannot reach the hub. Try again.
        </p>
      );
    case "error":
      return (
        <p role="alert" className="flex items-center gap-2 text-xs text-crit">
          <AlertTriangle className="h-3.5 w-3.5 shrink-0" aria-hidden />
          {outcome.detail ?? "Sign-in failed."}
        </p>
      );
    case "signed-in":
      return null;
  }
}

export function LoginView() {
  const { session, login } = useLive();
  const router = useRouter();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [outcome, setOutcome] = useState<LoginOutcome | null>(null);

  const onSubmit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    setOutcome(null);
    const result = await login(username, password);
    if (result.kind === "signed-in") return;
    setSubmitting(false);
    setOutcome(result);
  };

  const leaving = submitting || session.kind === "signed-in";
  useEffect(() => {
    if (session.kind !== "signed-in") return;
    // Development HMR needs a new authenticated socket; production retains
    // the shared provider and stream during client-side navigation.
    if (process.env.NODE_ENV === "development" && submitting) window.location.replace("/");
    else router.replace("/");
  }, [session.kind, submitting, router]);

  return (
    <main className="flex min-h-dvh items-center justify-center bg-background p-6">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex items-center gap-2.5">
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md border border-border text-accent">
            <BrandMark className="h-4 w-4" aria-hidden />
          </span>
          <span className="text-sm font-semibold tracking-tight">Rigscope</span>
        </div>

        <div className="rounded-xl border border-border bg-card p-6">
          <h1 className="text-lg font-semibold tracking-tight">Sign in</h1>

          {session.kind === "auth-unavailable" ? (
            <div className="mt-5 flex flex-col gap-3">
              <p role="alert" className="flex items-start gap-2 text-xs text-warn">
                <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
                <span>
                  Sign-in is unavailable. Contact your administrator.
                </span>
              </p>
            </div>
          ) : (
            <form onSubmit={onSubmit} className="mt-5 flex flex-col gap-4">
              <div className="flex flex-col gap-1.5">
                <label htmlFor="login-username" className="text-xs font-medium">
                  Username
                </label>
                <input
                  id="login-username"
                  name="username"
                  type="text"
                  autoComplete="username"
                  required
                  value={username}
                  onChange={(event) => setUsername(event.target.value)}
                  className="rounded-md border border-border bg-background px-3 py-2 text-sm text-foreground placeholder:text-muted-foreground/70"
                  disabled={leaving}
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <label htmlFor="login-password" className="text-xs font-medium">
                  Password
                </label>
                <input
                  id="login-password"
                  name="password"
                  type="password"
                  autoComplete="current-password"
                  required
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                  className="rounded-md border border-border bg-background px-3 py-2 text-sm text-foreground placeholder:text-muted-foreground/70"
                  disabled={leaving}
                />
              </div>
              {outcome ? <OutcomeMessage outcome={outcome} /> : null}
              <button
                type="submit"
                disabled={leaving}
                className="mt-1 inline-flex items-center justify-center rounded-md bg-accent px-3 py-2 text-sm font-medium text-accent-foreground transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {leaving ? "Signing in…" : "Sign in"}
              </button>
            </form>
          )}
          <footer className="mt-5 text-center"><SourceLink /></footer>
        </div>
      </div>
    </main>
  );
}
