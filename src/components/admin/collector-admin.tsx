"use client";

import { useCallback, useEffect, useId, useRef, useState } from "react";
import {
  AlertTriangle,
  ClipboardCopy,
  KeyRound,
  RefreshCw,
  ShieldOff,
  X,
} from "lucide-react";

import type { AdminApi, CollectorRecord, EnrollmentGrant } from "@/admin/api";
import {
  formatAdminTimestamp,
  formatMachineLabel,
  enrollmentTargets,
  grantCommand,
  type GrantMode,
  type AdminMachine,
} from "@/admin/presentation";
import { EmptyState, Panel } from "@/components/panel";
import {
  ADMIN_BUTTON_DANGER,
  ADMIN_BUTTON_PRIMARY,
  ADMIN_BUTTON_QUIET,
  ADMIN_BUTTON_SECONDARY,
  ADMIN_INPUT,
  Badge,
  Field,
  InlineNotice,
  friendlyError,
  isAbort,
  isForbidden,
  isUnauthorized,
} from "./admin-ui";

const TTL_OPTIONS = [
  { value: 5 * 60_000, label: "5 minutes" },
  { value: 10 * 60_000, label: "10 minutes" },
  { value: 15 * 60_000, label: "15 minutes" },
] as const;

export function CollectorAdmin(props: {
  api: AdminApi;
  csrfToken: string;
  sessionKey: string;
  machines: AdminMachine[];
  onAuditChange: () => void;
  onSessionLost: () => void;
}) {
  const { api, csrfToken, sessionKey, machines, onAuditChange, onSessionLost } = props;
  const prefix = useId();
  const fid = (name: string) => `${prefix}-${name}`;
  const [collectors, setCollectors] = useState<CollectorRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const [targetMachineId, setTargetMachineId] = useState("");
  const [ttlMs, setTtlMs] = useState<number>(15 * 60_000);
  const [grant, setGrant] = useState<
    (EnrollmentGrant & { mode: GrantMode }) | null
  >(null);
  const [grantError, setGrantError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [copyStatus, setCopyStatus] = useState("");
  const [confirmRevoke, setConfirmRevoke] = useState<CollectorRecord | null>(null);
  const [revoking, setRevoking] = useState(false);
  const [revokeError, setRevokeError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const mountedRef = useRef(true);
  const grantInputRef = useRef<HTMLInputElement | null>(null);
  const createRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      abortRef.current?.abort();
    };
  }, []);

  useEffect(() => {
    // Never keep a one-time secret visible across a sign-in/session change.
    setGrant(null);
    setCopyStatus("");
  }, [sessionKey]);

  useEffect(() => {
    if (!grant) return;
    grantInputRef.current?.focus();
    grantInputRef.current?.select();
  }, [grant]);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoading(true);
    setLoadError(null);
    try {
      const next = await api.listCollectors(controller.signal);
      if (!controller.signal.aborted) setCollectors(next);
    } catch (error) {
      if (controller.signal.aborted || isAbort(error)) return;
      if (isUnauthorized(error)) {
        onSessionLost();
        return;
      }
      setLoadError(
        isForbidden(error)
          ? "Administrator access is required to inspect collector mappings."
          : friendlyError(error, "Could not load the collector registry."),
      );
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [api, onSessionLost]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (targetMachineId === "") return;
    if (!collectors.some((row) => row.machineId === targetMachineId)) {
      setTargetMachineId("");
    }
  }, [collectors, targetMachineId]);

  const targets = enrollmentTargets(collectors);
  const selectedTarget = targets.find((target) => target.machineId === targetMachineId);
  const replacementMode = selectedTarget !== undefined;
  const createdCommand = grant ? grantCommand(grant.mode, window.location.origin) : null;

  const createGrant = async () => {
    if (creating) return;
    setGrantError(null);
    setCreating(true);
    const mode: GrantMode = selectedTarget?.mode ?? "new";
    try {
      const created = await api.createGrant(
        {
          ttlMs,
          ...(replacementMode ? { replaceMachineId: targetMachineId } : {}),
        },
        csrfToken,
      );
      if (!mountedRef.current) return;
      setGrant({ ...created, mode });
      setCopyStatus("");
      setAnnouncement(
        "Grant created. Copy before leaving.",
      );
      onAuditChange();
    } catch (error) {
      if (!mountedRef.current) return;
      if (isUnauthorized(error)) {
        onSessionLost();
        return;
      }
      if (isForbidden(error)) {
        setGrantError(
          replacementMode
            ? "You do not have access to that machine, so a replacement grant cannot be created."
            : "A new installation needs a hub administrator with all-machine scope. Choose an existing mapping for a replacement grant, or ask the hub owner to widen your scope.",
        );
        return;
      }
      setGrantError(friendlyError(error, "Could not create an enrollment grant."));
    } finally {
      if (mountedRef.current) setCreating(false);
    }
  };

  const copyGrant = async () => {
    if (!grant) return;
    try {
      if (!navigator.clipboard?.writeText) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(grant.token);
      setCopyStatus("Grant copied to the clipboard.");
    } catch {
      setCopyStatus("Copy failed. Select the grant field and copy it manually.");
    }
  };

  const copyCommand = async () => {
    if (!createdCommand) return;
    try {
      if (!navigator.clipboard?.writeText) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(createdCommand.command);
      setCopyStatus("Command copied.");
    } catch {
      setCopyStatus("Copy failed. Select the command and copy it manually.");
    }
  };

  const dismissGrant = () => {
    setGrant(null);
    setCopyStatus("");
    setGrantError(null);
    setAnnouncement("Grant dismissed.");
    requestAnimationFrame(() => createRef.current?.focus());
  };

  const revoke = async () => {
    if (!confirmRevoke || revoking) return;
    setRevoking(true);
    setRevokeError(null);
    try {
      await api.revokeCollector(confirmRevoke.collectorId, csrfToken);
      if (!mountedRef.current) return;
      const label = `${confirmRevoke.collectorId} on ${confirmRevoke.machineId}`;
      setConfirmRevoke(null);
      setAnnouncement(`Revoked ${label}. Its live stream was disconnected.`);
      onAuditChange();
      await load();
    } catch (error) {
      if (!mountedRef.current) return;
      if (isUnauthorized(error)) {
        onSessionLost();
        return;
      }
      setRevokeError(
        isForbidden(error)
          ? "Your account does not have access to that collector's machine."
          : friendlyError(error, "Could not revoke that collector."),
      );
    } finally {
      if (mountedRef.current) setRevoking(false);
    }
  };

  const machineLabel = (machineId: string): string => {
    const machine = machines.find((item) => item.machineId === machineId);
    return machine ? formatMachineLabel(machine) : machineId;
  };

  return (
    <Panel
      title="Collectors"
      className="min-w-0"
      action={
        <button type="button" className={ADMIN_BUTTON_QUIET} onClick={() => void load()} disabled={loading}>
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} aria-hidden />
          Refresh
        </button>
      }
    >
      <p
        role="status"
        className={
          announcement
            ? "mb-3 rounded-md bg-ok-soft px-3 py-2 text-xs text-foreground"
            : "sr-only"
        }
      >
        {announcement}
      </p>

      {loadError ? (
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <InlineNotice tone="error" role="alert">
            {loadError}
          </InlineNotice>
          <button type="button" className={ADMIN_BUTTON_SECONDARY} onClick={() => void load()}>
            Try again
          </button>
        </div>
      ) : null}

      <div className="grid min-w-0 gap-6 lg:grid-cols-2">
        <section aria-labelledby={fid("registry-heading")} className="min-w-0">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h3 id={fid("registry-heading")} className="text-sm font-medium tracking-tight">
              Registry
            </h3>
            <span className="tnum text-xs text-muted-foreground">
              {collectors.filter((row) => !row.revoked).length} active / {collectors.length} total
            </span>
          </div>

          {loading && collectors.length === 0 ? (
            <p role="status" className="py-6 text-sm text-muted-foreground">
              Loading collector mappings…
            </p>
          ) : null}

          {!loading && !loadError && collectors.length === 0 ? (
            <EmptyState
              icon={ShieldOff}
              note="No collector mappings are visible."
            />
          ) : null}

          {collectors.length > 0 ? (
            <ul className="mt-2 divide-y divide-border border-t border-border">
              {collectors.map((collector) => {
                const confirming = confirmRevoke?.collectorId === collector.collectorId;
                return (
                  <li key={collector.collectorId} className="min-w-0 py-3">
                    <div className="flex min-w-0 flex-wrap items-start justify-between gap-x-4 gap-y-2">
                      <div className="min-w-0">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <span className="break-words text-sm font-medium">
                            {machineLabel(collector.machineId)}
                          </span>
                          {collector.revoked ? (
                            <Badge tone="crit">Revoked</Badge>
                          ) : (
                            <Badge tone="ok">Active</Badge>
                          )}
                        </div>
                        <dl className="mt-1 flex flex-col gap-0.5 text-xs text-muted-foreground">
                          <div className="flex min-w-0 gap-1.5">
                            <dt className="shrink-0">Machine</dt>
                            <dd className="tnum break-all">{collector.machineId}</dd>
                          </div>
                          <div className="flex min-w-0 gap-1.5">
                            <dt className="shrink-0">Collector</dt>
                            <dd className="tnum break-all">{collector.collectorId}</dd>
                          </div>
                          <div className="flex min-w-0 gap-1.5">
                            <dt className="shrink-0">Updated</dt>
                            <dd>{formatAdminTimestamp(collector.updatedAt)}</dd>
                          </div>
                        </dl>
                      </div>
                      {!collector.revoked ? (
                        <div className="flex flex-wrap items-center gap-1">
                          <button
                            type="button"
                            className={ADMIN_BUTTON_QUIET}
                            onClick={() => {
                              setTargetMachineId(collector.machineId);
                              setGrantError(null);
                              requestAnimationFrame(() => createRef.current?.focus());
                            }}
                          >
                            <KeyRound className="h-3.5 w-3.5" aria-hidden />
                            Replacement grant
                          </button>
                          <button
                            type="button"
                            className={ADMIN_BUTTON_QUIET}
                            aria-expanded={confirming}
                            onClick={() => {
                              setRevokeError(null);
                              setConfirmRevoke(confirming ? null : collector);
                            }}
                          >
                            <ShieldOff className="h-3.5 w-3.5" aria-hidden />
                            Revoke
                          </button>
                        </div>
                      ) : (
                        <div className="flex flex-wrap items-center gap-1">
                          <button
                            type="button"
                            className={ADMIN_BUTTON_QUIET}
                            onClick={() => {
                              setTargetMachineId(collector.machineId);
                              setGrantError(null);
                              requestAnimationFrame(() => createRef.current?.focus());
                            }}
                          >
                            <KeyRound className="h-3.5 w-3.5" aria-hidden />
                            Re-enroll
                          </button>
                        </div>
                      )}
                    </div>

                    {confirming ? (
                      <div
                        role="alert"
                        className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-md bg-crit-soft px-3 py-2"
                      >
                        <p className="min-w-0 flex-1 text-xs text-foreground">
                          Revoke <strong className="font-medium">{collector.collectorId}</strong>{" "}
                          on <strong className="font-medium">{machineLabel(collector.machineId)}</strong>?
                          This disconnects the collector and invalidates its credential. To bring it
                          back later, create a re-enrollment grant for this machine.
                        </p>
                        <div className="flex flex-wrap gap-1.5">
                          <button
                            type="button"
                            className={ADMIN_BUTTON_DANGER}
                            onClick={() => void revoke()}
                            disabled={revoking}
                          >
                            {revoking ? "Revoking…" : "Revoke collector"}
                          </button>
                          <button
                            type="button"
                            className={ADMIN_BUTTON_SECONDARY}
                            onClick={() => setConfirmRevoke(null)}
                            disabled={revoking}
                          >
                            Cancel
                          </button>
                        </div>
                      </div>
                    ) : null}
                    {confirming && revokeError ? (
                      <p role="alert" className="mt-2 text-xs text-crit">
                        {revokeError}
                      </p>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          ) : null}
        </section>

        <section
          aria-labelledby={fid("grant-heading")}
          className="flex min-w-0 flex-col gap-3 border-t border-border pt-5 lg:border-l lg:border-t-0 lg:pl-6 lg:pt-0"
        >
          <div>
            <h3 id={fid("grant-heading")} className="text-sm font-medium tracking-tight">
              Add machine
            </h3>
          </div>

          <Field id={fid("target")} label="Enrollment target">
            <select
              id={fid("target")}
              value={targetMachineId}
              onChange={(event) => {
                setTargetMachineId(event.target.value);
                setGrantError(null);
              }}
              className={ADMIN_INPUT}
            >
              <option value="">New machine</option>
              {targets.map((target) => (
                <option key={target.collectorId} value={target.machineId}>
                  {target.mode === "reenroll" ? "Re-enroll revoked" : "Replacement"} · {machineLabel(target.machineId)} · {target.collectorId}
                </option>
              ))}
            </select>
          </Field>

          <Field id={fid("ttl")} label="Grant lifetime" >
            <select
              id={fid("ttl")}
              value={ttlMs}
              onChange={(event) => setTtlMs(Number(event.target.value))}
              className={ADMIN_INPUT}
            >
              {TTL_OPTIONS.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
          </Field>

          {grantError ? (
            <InlineNotice tone="error" role="alert">
              {grantError}
            </InlineNotice>
          ) : null}

          <div className="flex flex-wrap items-center gap-2">
            <button
              ref={createRef}
              type="button"
              className={ADMIN_BUTTON_PRIMARY}
              onClick={() => void createGrant()}
              disabled={creating}
              aria-busy={creating}
            >
              <KeyRound className="h-3.5 w-3.5" aria-hidden />
              {creating
                ? "Creating…"
                : selectedTarget?.mode === "reenroll"
                  ? "Create re-enrollment grant"
                  : replacementMode
                  ? "Create replacement grant"
                  : "Create connection grant"}
            </button>
          </div>

          {grant && createdCommand ? (
            <section
              aria-labelledby={fid("grant-reveal-heading")}
              className="flex min-w-0 flex-col gap-3 border-t border-border pt-3"
            >
              <div className="flex flex-wrap items-start justify-between gap-2">
                <h4 id={fid("grant-reveal-heading")} className="text-sm font-medium">
                  Copy this grant now
                </h4>
                <button
                  type="button"
                  className={ADMIN_BUTTON_QUIET}
                  onClick={dismissGrant}
                  aria-label="Dismiss enrollment grant"
                >
                  <X className="h-3.5 w-3.5" aria-hidden />
                  Dismiss
                </button>
              </div>

              <InlineNotice tone="warn" role="alert">
                <span className="flex items-start gap-2">
                  <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
                  <span>
                    Single-use grant · expires{" "}
                    <time dateTime={grant.expiresAt}>{formatAdminTimestamp(grant.expiresAt)}</time>. Copy before leaving.
                  </span>
                </span>
              </InlineNotice>

              <div className="flex min-w-0 flex-col gap-1.5">
                <label htmlFor={fid("grant-token")} className="text-xs font-medium">
                  One-time grant
                </label>
                <div className="flex min-w-0 flex-wrap gap-1.5">
                  <input
                    ref={grantInputRef}
                    id={fid("grant-token")}
                    readOnly
                    autoComplete="off"
                    spellCheck={false}
                    value={grant.token}
                    onFocus={(event) => event.currentTarget.select()}
                    className={`${ADMIN_INPUT} tnum min-w-0 flex-1 break-all`}
                  />
                  <button type="button" className={ADMIN_BUTTON_SECONDARY} onClick={() => void copyGrant()}>
                    <ClipboardCopy className="h-3.5 w-3.5" aria-hidden />
                    Copy grant
                  </button>
                </div>
                <p className="sr-only" role="status">
                  {copyStatus}
                </p>
                {copyStatus ? <p className="text-xs text-muted-foreground">{copyStatus}</p> : null}
              </div>

              <div className="min-w-0">
                <p className="text-xs font-medium">Run on the new machine</p>
                <pre className="mt-1.5 overflow-x-auto whitespace-pre-wrap break-all rounded-md bg-card-strong p-3 text-xs text-foreground">
                  <code>{createdCommand.command}</code>
                </pre>
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <button type="button" className={ADMIN_BUTTON_SECONDARY} onClick={() => void copyCommand()}>
                    <ClipboardCopy className="h-3.5 w-3.5" aria-hidden />Copy command
                  </button>
                </div>
                <p className="mt-2 text-xs text-muted-foreground">{createdCommand.note}</p>
              </div>
            </section>
          ) : null}
        </section>
      </div>
    </Panel>
  );
}
