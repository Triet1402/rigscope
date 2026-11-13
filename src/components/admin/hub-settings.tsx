"use client";

/**
 * Administration → Hub settings.
 *
 * A read-only summary of what the running hub started with, plus a storage
 * mode form. The hub runs unprivileged: it cannot rewrite its configuration
 * or restart itself, and it never touches collector state. The form therefore
 * ends in a terminal handoff — the exact `rigscope configure` command to run
 * on the hub machine, which previews the change and asks before restarting.
 */

import { useCallback, useEffect, useId, useRef, useState } from "react";
import { AlertTriangle, ClipboardCopy, RefreshCw, Settings2 } from "lucide-react";

import type { AdminApi, HubSettings } from "@/admin/api";
import {
  STORAGE_CHOICES,
  configurePrefix,
  familyList,
  formatInterval,
  storageChangeWarning,
  storageCommand,
} from "@/admin/settings";
import { formatAdminTimestamp } from "@/admin/presentation";
import type { HubStorageMode } from "@/contracts/api.generated";
import { EmptyState, Panel } from "@/components/panel";
import { formatBytes } from "@/live/presentation";
import {
  ADMIN_BUTTON_QUIET,
  ADMIN_BUTTON_SECONDARY,
  Badge,
  InlineNotice,
  friendlyError,
  isAbort,
  isForbidden,
  isUnauthorized,
} from "./admin-ui";

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid min-w-0 grid-cols-[minmax(7rem,9rem)_minmax(0,1fr)] gap-x-3 gap-y-0.5 py-1.5 text-xs">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-words text-foreground">{children}</dd>
    </div>
  );
}

export function HubSettingsPanel(props: {
  api: AdminApi;
  sessionKey: string;
  onSessionLost: () => void;
}) {
  const { api, sessionKey, onSessionLost } = props;
  const id = useId();
  const [settings, setSettings] = useState<HubSettings | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<HubStorageMode | null>(null);
  const [copyStatus, setCopyStatus] = useState("");
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoading(true);
    setError(null);
    try {
      const next = await api.getSettings(controller.signal);
      if (controller.signal.aborted) return;
      setSettings(next);
      setSelected(next.storage.mode);
      setCopyStatus("");
    } catch (caught) {
      if (controller.signal.aborted || isAbort(caught)) return;
      if (isUnauthorized(caught)) {
        onSessionLost();
        return;
      }
      setError(
        isForbidden(caught)
          ? "Administrator access is required to read hub settings."
          : friendlyError(caught, "Could not load hub settings."),
      );
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [api, onSessionLost]);

  useEffect(() => {
    void load();
    return () => abortRef.current?.abort();
  }, [load, sessionKey]);

  const current = settings?.storage.mode ?? null;
  const target = selected ?? current;
  const warning = current && target ? storageChangeWarning(current, target) : null;
  const command = settings && target && warning ? storageCommand(settings.scope, target) : null;

  async function copyCommand() {
    if (!command) return;
    try {
      if (!navigator.clipboard?.writeText) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(command);
      setCopyStatus("Command copied.");
    } catch {
      setCopyStatus("Copy failed. Select the command and copy it manually.");
    }
  }

  return (
    <Panel
      title="Hub settings"
      className="min-w-0"
      action={
        <button type="button" className={ADMIN_BUTTON_QUIET} onClick={() => void load()} disabled={loading}>
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} aria-hidden />
          Refresh
        </button>
      }
    >
      {error ? (
        <InlineNotice tone="error" role="alert">
          {error}
        </InlineNotice>
      ) : null}

      {!settings && !error ? <EmptyState icon={Settings2} note={loading ? "Loading hub settings…" : "No settings to show."} /> : null}

      {settings ? (
        <div className="flex min-w-0 flex-col gap-5">
          <p className="text-xs text-muted-foreground">
            The running hub and the collector&apos;s saved settings. Changes are made in a terminal on the hub machine, because the dashboard
            cannot restart the hub or change this machine&apos;s files.
          </p>

          <div className="grid min-w-0 gap-5 md:grid-cols-2">
            <section aria-labelledby={`${id}-network`} className="min-w-0">
              <h3 id={`${id}-network`} className="text-xs font-medium text-foreground">
                Dashboard address
              </h3>
              <dl className="mt-1 divide-y divide-border">
                <Row label="Opens at">
                  <span className="break-all">{settings.network.origin ?? settings.network.url}</span>
                </Row>
                <Row label="Listens on">
                  <span className="tnum">
                    {settings.network.listen} port {settings.network.port}
                  </span>
                </Row>
                <Row label="HTTPS">
                  {settings.network.tls ? "Served by the hub" : "Not served by the hub (a proxy, or local only)"}
                </Row>
                <Row label="Trusted proxies">{familyList(settings.network.trustedProxies)}</Row>
                {settings.network.trustedProxies.length > 0 ? (
                  <Row label="Proxy headers">{settings.network.forwardedHeaders}</Row>
                ) : null}
              </dl>
            </section>

            <section aria-labelledby={`${id}-collector`} className="min-w-0">
              <h3 id={`${id}-collector`} className="text-xs font-medium text-foreground">
                This machine&apos;s collector
              </h3>
              {settings.localCollector ? (
                <dl className="mt-1 divide-y divide-border">
                  <Row label="Samples every">
                    <span className="tnum">{formatInterval(settings.localCollector.samplingIntervalMs)}</span>
                  </Row>
                  <Row label="Sends to">
                    <span className="break-all">{settings.localCollector.destination ?? "No hub yet"}</span>
                  </Row>
                  <Row label="Shares">{familyList(settings.localCollector.shared)}</Row>
                  <Row label="Keeps private">{familyList(settings.localCollector.withheld)}</Row>
                  <Row label="GPU registers">{settings.localCollector.gpuRegisters ? "On" : "Off"}</Row>
                </dl>
              ) : (
                <p className="mt-2 text-xs text-muted-foreground">
                  Not shown: no collector runs on the hub machine, or your access covers only some machines.
                </p>
              )}
              <p className="mt-2 text-xs text-muted-foreground">
                Saved settings are read when the collector starts. A stopped collector sends nothing.
                Every machine applies its own sharing settings before anything is sent; change them on that machine.
              </p>
            </section>
          </div>

          <form
            className="flex min-w-0 flex-col gap-3 border-t border-border pt-4"
            aria-describedby={`${id}-storage-usage`}
            onSubmit={(event) => {
              event.preventDefault();
              void copyCommand();
            }}
          >
            <fieldset className="flex min-w-0 flex-col gap-2">
              <legend className="text-xs font-medium text-foreground">History storage</legend>
              <p id={`${id}-storage-usage`} className="tnum text-xs text-muted-foreground">
                On disk {formatBytes(settings.storage.diskBytes)}
                {settings.storage.diskLimitBytes !== null ? ` of ${formatBytes(settings.storage.diskLimitBytes)}` : ""}
                {" · "}in memory {formatBytes(settings.storage.memoryBytes)} of {formatBytes(settings.storage.memoryBudgetBytes)}
                {settings.storage.oldestHistoryMs !== null
                  ? ` · history since ${formatAdminTimestamp(new Date(settings.storage.oldestHistoryMs).toISOString())}`
                  : " · no history yet"}
              </p>
              <div className="grid min-w-0 gap-2 sm:grid-cols-2">
                {STORAGE_CHOICES.map((choice) => {
                  const inputId = `${id}-storage-${choice.mode}`;
                  const checked = target === choice.mode;
                  return (
                    <label
                      key={choice.mode}
                      htmlFor={inputId}
                      className={`flex min-w-0 cursor-pointer items-start gap-2.5 rounded-lg border p-3 text-xs transition-colors ${
                        checked ? "border-accent bg-selected" : "border-border hover:bg-card-strong"
                      }`}
                    >
                      <input
                        id={inputId}
                        type="radio"
                        name={`${id}-storage`}
                        value={choice.mode}
                        checked={checked}
                        onChange={() => {
                          setSelected(choice.mode);
                          setCopyStatus("");
                        }}
                        aria-describedby={`${inputId}-summary`}
                        className="mt-0.5 accent-accent"
                      />
                      <span className="flex min-w-0 flex-col gap-1">
                        <span className="flex flex-wrap items-center gap-1.5 font-medium text-foreground">
                          {choice.label}
                          {current === choice.mode ? <Badge tone="accent">Current</Badge> : null}
                        </span>
                        <span id={`${inputId}-summary`} className="text-muted-foreground">
                          {choice.summary}
                        </span>
                      </span>
                    </label>
                  );
                })}
              </div>
            </fieldset>

            {warning && command ? (
              <div className="flex min-w-0 flex-col gap-2">
                <InlineNotice tone="warn" role="status">
                  <span className="flex items-start gap-2">
                    <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
                    <span>{warning}</span>
                  </span>
                </InlineNotice>
                <p className="text-xs font-medium">Run on the hub machine</p>
                <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded-md bg-card-strong p-3 text-xs text-foreground">
                  <code>{command}</code>
                </pre>
                <div className="flex flex-wrap items-center gap-2">
                  <button type="submit" className={ADMIN_BUTTON_SECONDARY}>
                    <ClipboardCopy className="h-3.5 w-3.5" aria-hidden />
                    Copy command
                  </button>
                  {copyStatus ? <span className="text-xs text-muted-foreground">{copyStatus}</span> : null}
                </div>
                <p className="sr-only" role="status">
                  {copyStatus}
                </p>
                <p className="text-xs text-muted-foreground">
                  It shows the change and asks before restarting the hub. This page reconnects when the hub is back;
                  then select Refresh.
                </p>
              </div>
            ) : (
              <p className="text-xs text-muted-foreground">Choose the other option to get the command that switches.</p>
            )}
          </form>

          <p className="text-xs text-muted-foreground">
            See every setting, including ones not listed here:{" "}
            <code className="rounded bg-card-strong px-1.5 py-0.5 text-foreground">{configurePrefix(settings.scope)} --show</code>
          </p>
        </div>
      ) : null}
    </Panel>
  );
}
