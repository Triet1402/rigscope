"use client";

/**
 * Alerts surface: scoped incidents, rules, notification state and an explicit
 * admin-only rule/webhook form. Viewers see only their machine scope and never
 * receive webhook URLs, headers or secrets.
 */
import { useCallback, useEffect, useState } from "react";
import {
  Bell,
  Inbox,
  Pencil,
  RefreshCw,
  ShieldAlert,
  Trash2,
} from "lucide-react";

import { AppShell } from "@/components/shell/app-shell";
import { EmptyState, Panel } from "@/components/panel";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import {
  StreamBanners,
  useAccessGate,
} from "@/components/live/stream-banners";
import { formatAge, ageSince } from "@/live/presentation";
import { useLive } from "@/live/live-provider";
import { useNow } from "@/live/use-now";
import {
  type AlertIncident,
  type AlertListResponse,
  type AlertNotification,
  type AlertRule,
  type AlertRuleInput,
  type AlertWebhookConfigureInput,
  type AlertWebhookResponse,
  type AlertWebhookTestResponse,
  type AlertWebhookTestResult,
} from "@/alerts/types";
import {
  alertConditionLabel,
  alertConditionNeedsThreshold,
  alertMetricLabel,
  formatAlertDuration,
  formatMetricValue,
} from "@/alerts/format";

import { AlertRuleForm } from "./rule-form";
import { AlertWebhookPanel } from "./webhook-panel";

function formatWhen(iso: string | null): string {
  if (iso === null) return "—";
  const date = new Date(iso);
  return Number.isFinite(date.getTime()) ? date.toLocaleString() : iso;
}

function StatusBadge({
  tone,
  children,
}: {
  tone: "ok" | "warn" | "crit" | "muted";
  children: React.ReactNode;
}) {
  const className =
    tone === "ok" ? "bg-ok-soft text-ok"
    : tone === "warn" ? "bg-warn-soft text-warn"
    : tone === "crit" ? "bg-crit-soft text-crit"
    : "border border-border text-muted-foreground";
  return (
    <span className={`tnum inline-flex items-center rounded-full px-2 py-0.5 text-xs ${className}`}>
      {children}
    </span>
  );
}

function IncidentRow({ incident, now }: { incident: AlertIncident; now: number }) {
  const open = incident.status === "open";
  return (
    <li className="border-b border-border py-3 last:border-b-0">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <StatusBadge tone={open ? "crit" : "ok"}>{open ? "Open" : "Recovered"}</StatusBadge>
            <span className="truncate text-sm font-medium">{incident.ruleName}</span>
            <span className="tnum text-xs text-muted-foreground">{incident.machineId}</span>
          </div>
          <p className="mt-1 max-w-2xl text-sm">{incident.message}</p>
          <p className="mt-1 text-xs text-muted-foreground">
            {alertMetricLabel(incident.metric, incident.resourceId)} · started{" "}
            {formatAge(ageSince(incident.startedAt, now))}
            {incident.resolvedAt !== null
              ? ` · recovered ${formatAge(ageSince(incident.resolvedAt, now))}`
              : ""}
          </p>
        </div>
        <dl className="tnum grid shrink-0 grid-cols-2 gap-x-4 gap-y-1 text-xs text-muted-foreground">
          <dt>Started value</dt>
          <dd className="text-right text-foreground">
            {formatMetricValue(incident.metric, incident.startedValue)}
          </dd>
          <dt>Recovered value</dt>
          <dd className="text-right text-foreground">
            {formatMetricValue(incident.metric, incident.resolvedValue)}
          </dd>
        </dl>
      </div>
    </li>
  );
}

function RuleRow({
  rule,
  admin,
  busy,
  confirmingDelete,
  onEdit,
  onRequestDelete,
  onCancelDelete,
  onConfirmDelete,
}: {
  rule: AlertRule;
  admin: boolean;
  busy: boolean;
  confirmingDelete: boolean;
  onEdit: () => void;
  onRequestDelete: () => void;
  onCancelDelete: () => void;
  onConfirmDelete: () => Promise<void>;
}) {
  const condition = alertConditionNeedsThreshold(rule.condition)
    ? `${alertConditionLabel(rule.condition)} ${formatMetricValue(rule.metric, rule.threshold)}`
    : alertConditionLabel(rule.condition);
  const stateTone = !rule.enabled ? "muted" : rule.state === "firing" ? "crit" : rule.state === "pending" ? "warn" : "ok";
  const stateLabel = !rule.enabled ? "Disabled" : rule.state === "firing" ? "Firing" : rule.state === "pending" ? "Pending" : "OK";
  return (
    <li className="border-b border-border py-3 last:border-b-0">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="truncate text-sm font-medium">{rule.name}</span>
            <StatusBadge tone={stateTone}>{stateLabel}</StatusBadge>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            {rule.machineId} · {alertMetricLabel(rule.metric, rule.resourceId)}
          </p>
          <p className="mt-0.5 text-xs text-muted-foreground">
            {condition} · holds {formatAlertDuration(rule.durationMs)}
            {rule.pendingSince ? ` · pending since ${formatWhen(rule.pendingSince)}` : ""}
          </p>
          {rule.maintenanceUntil ? (
            <p className="mt-0.5 text-xs text-muted-foreground">
              Maintenance until {formatWhen(rule.maintenanceUntil)}
            </p>
          ) : null}
        </div>
        {admin ? (
          <div className="flex shrink-0 items-center gap-1.5">
            <button
              type="button"
              onClick={onEdit}
              disabled={busy || confirmingDelete}
              className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
            >
              <Pencil className="h-3 w-3" aria-hidden />
              Edit
            </button>
            <button
              type="button"
              onClick={onRequestDelete}
              disabled={busy || confirmingDelete}
              className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-crit transition-colors hover:bg-crit-soft disabled:opacity-50"
            >
              <Trash2 className="h-3 w-3" aria-hidden />
              Delete
            </button>
          </div>
        ) : null}
      </div>
      {confirmingDelete ? (
        <div
          role="group"
          aria-label="Confirm rule deletion"
          className="mt-3 rounded-md bg-warn-soft px-3 py-2.5"
        >
          <p className="text-xs text-foreground">
            Delete “{rule.name}”? Its incident history stays, but the rule stops evaluating.
          </p>
          <div className="mt-2 flex flex-wrap gap-2">
            <button
              type="button"
              autoFocus
              onClick={() => void onConfirmDelete()}
              disabled={busy}
              className="rounded-md bg-crit px-2.5 py-1 text-xs font-medium text-accent-foreground transition-colors disabled:opacity-50"
            >
              Confirm delete
            </button>
            <button
              type="button"
              onClick={onCancelDelete}
              disabled={busy}
              className="rounded-md border border-border px-2.5 py-1 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
            >
              Cancel
            </button>
          </div>
        </div>
      ) : null}
    </li>
  );
}

function NotificationRow({ notification }: { notification: AlertNotification }) {
  const tone =
    notification.status === "failed" ? "crit" : notification.status === "delivered" ? "ok" : "warn";
  return (
    <li className="border-b border-border py-2 last:border-b-0">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 text-xs">
        <span className="flex items-center gap-2">
          <StatusBadge tone={tone}>{notification.status}</StatusBadge>
          <span className="text-foreground">{notification.event}</span>
          <span className="tnum text-muted-foreground">
            attempt {notification.attempts}/{notification.maxAttempts}
          </span>
        </span>
        <span className="tnum text-muted-foreground">{formatWhen(notification.createdAt)}</span>
      </div>
      {notification.lastError ? (
        <p className="mt-1 text-xs text-warn">{notification.lastError}</p>
      ) : null}
      <p className="tnum mt-0.5 truncate font-mono text-xs text-muted-foreground">
        {notification.id}
      </p>
    </li>
  );
}

export function AlertsView() {
  const { session, machines } = useLive();
  const gate = useAccessGate();
  const now = useNow();
  const csrfToken = session.kind === "signed-in" ? session.user.csrfToken : null;
  const sessionAdmin =
    session.kind === "signed-in" && (session.user as {admin?: boolean}).admin === true;
  const [data, setData] = useState<AlertListResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [editing, setEditing] = useState<AlertRule | null>(null);
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const admin = sessionAdmin && (data === null || data.canAdmin);

  const load = useCallback(async () => {
    if (csrfToken === null) return;
    setLoading(true);
    setError(null);
    try {
      const response = await fetch("/api/alerts", {
        headers: { Accept: "application/json" },
        cache: "no-store",
        credentials: "same-origin",
      });
      const body = (await response.json().catch(() => ({}))) as AlertListResponse & {error?: string};
      if (!response.ok) throw new Error(body.error ?? `Could not load alerts (${response.status}).`);
      setData(body);
    } catch (loadError) {
      setError(loadError instanceof Error ? loadError.message : "Could not load alerts.");
    } finally {
      setLoading(false);
    }
  }, [csrfToken]);

  useEffect(() => {
    if (csrfToken !== null) void load();
  }, [csrfToken, load]);

  const mutate = useCallback(
    async <T,>(path: string, init: RequestInit): Promise<T> => {
      if (csrfToken === null) throw new Error("Sign in required.");
      setBusy(true);
      setError(null);
      try {
        const response = await fetch(path, {
          ...init,
          cache: "no-store",
          credentials: "same-origin",
          headers: {
            Accept: "application/json",
            "Content-Type": "application/json",
            "x-csrf-token": csrfToken,
            ...(init.headers ?? {}),
          },
        });
        const body = (await response.json().catch(() => ({}))) as T & {error?: string};
        if (!response.ok) throw new Error(body.error ?? `Request failed (${response.status}).`);
        return body;
      } finally {
        setBusy(false);
      }
    },
    [csrfToken],
  );

  const saveRule = async (input: AlertRuleInput) => {
    await mutate<{rule: AlertRule}>("/api/alerts/rules", {
      method: "POST",
      body: JSON.stringify(input),
    });
    setEditing(null);
    setNotice(input.id ? "Rule saved." : "Rule created.");
    await load();
  };

  const deleteRule = async (rule: AlertRule) => {
    try {
      await mutate<{ok: boolean}>(`/api/alerts/rules?id=${encodeURIComponent(rule.id)}`, {
        method: "DELETE",
      });
      if (editing?.id === rule.id) setEditing(null);
      setConfirmDeleteId(null);
      setNotice("Rule deleted.");
      await load();
    } catch (deleteError) {
      setError(deleteError instanceof Error ? deleteError.message : "Could not delete the rule.");
    }
  };

  const configureWebhook = async (input: AlertWebhookConfigureInput) => {
    await mutate<AlertWebhookResponse>("/api/alerts/webhook", {
      method: "POST",
      body: JSON.stringify({action: "configure", ...input}),
    });
    setNotice("Webhook configured.");
    await load();
  };

  const clearWebhook = async () => {
    await mutate<AlertWebhookResponse>("/api/alerts/webhook", {
      method: "POST",
      body: JSON.stringify({action: "clear"}),
    });
    setNotice("Webhook cleared.");
    await load();
  };

  const testWebhook = async (): Promise<AlertWebhookTestResult> => {
    const response = await mutate<AlertWebhookTestResponse>("/api/alerts/webhook", {
      method: "POST",
      body: JSON.stringify({action: "test"}),
    });
    setData((previous) => previous === null ? previous : {...previous, webhook: response.status});
    setNotice(response.result.ok ? "Webhook test delivered." : "Webhook test failed.");
    return response.result;
  };

  const incidents = data?.incidents ?? [];
  const openIncidents = incidents.filter((incident) => incident.status === "open").length;
  const rules = data?.rules ?? [];
  const notifications = data?.notifications;
  const showBusy = loading && data === null;

  return (
    <AppShell
      title="Alerts"
      headerRight={
        <>
          <LiveStatusChip />
          <UserMenu />
        </>
      }
    >
      {gate ? (
        gate
      ) : (
        <>
          <StreamBanners />
          <div className="mb-4 flex flex-wrap items-center justify-end gap-3">
            <button
              type="button"
              onClick={() => void load()}
              disabled={loading}
              className="inline-flex items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
            >
              <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} aria-hidden />
              Refresh
            </button>
          </div>

          <p role="status" className={notice ? "mb-4 text-xs text-muted-foreground" : "sr-only"}>
            {notice ?? ""}
          </p>
          {error ? (
            <p role="alert" className="mb-4 rounded-lg bg-crit-soft px-4 py-2.5 text-xs text-foreground">
              {error}
            </p>
          ) : null}

          {showBusy ? (
            <Panel title="Alerts" className="mt-8">
              <EmptyState
                icon={RefreshCw}
                note="Loading alert rules and incidents…"
              />
            </Panel>
          ) : data === null ? (
            <Panel title="Alerts unavailable" className="mt-8">
              <EmptyState
                icon={ShieldAlert}
                note="Alert state could not be loaded."
              />
            </Panel>
          ) : (
            <div className={admin ? "grid gap-5 xl:grid-cols-[minmax(0,1fr)_24rem]" : "flex flex-col gap-5"}>
              <div className="flex min-w-0 flex-col gap-5">
                <Panel
                  title="Incidents and recoveries"
                  action={
                    <span>
                      {openIncidents} open · {incidents.length} recent
                    </span>
                  }
                >
                  {incidents.length === 0 ? (
                    <EmptyState
                      icon={Inbox}
                      note="No incidents."
                    />
                  ) : (
                    <ul className="flex flex-col">
                      {incidents.map((incident) => (
                        <IncidentRow key={incident.id} incident={incident} now={now} />
                      ))}
                    </ul>
                  )}
                </Panel>

                <Panel
                  title="Rules"
                  action={<span>{rules.length} configured</span>}
                >
                  {rules.length === 0 ? (
                    <EmptyState
                      icon={Bell}
                      note="No rules."
                    />
                  ) : (
                    <ul className="flex flex-col">
                      {rules.map((rule) => (
                        <RuleRow
                          key={rule.id}
                          rule={rule}
                          admin={admin}
                          busy={busy}
                          confirmingDelete={confirmDeleteId === rule.id}
                          onEdit={() => {
                            setConfirmDeleteId(null);
                            setEditing(rule);
                          }}
                          onRequestDelete={() => setConfirmDeleteId(rule.id)}
                          onCancelDelete={() => setConfirmDeleteId(null)}
                          onConfirmDelete={() => deleteRule(rule)}
                        />
                      ))}
                    </ul>
                  )}
                </Panel>

                <Panel
                  title="Notifications"
                  action={
                    <span>
                      {notifications?.pending ?? 0} pending · {notifications?.failed ?? 0} failed
                    </span>
                  }
                >
                  {notifications === undefined || notifications.recent.length === 0 ? (
                    <p className="text-xs text-muted-foreground">
                      No notifications.
                    </p>
                  ) : (
                    <>
                      <dl className="grid grid-cols-3 gap-3 border-b border-border pb-3 text-xs">
                        <div>
                          <dt className="text-muted-foreground">Pending</dt>
                          <dd className="tnum text-foreground">{notifications.pending}</dd>
                        </div>
                        <div>
                          <dt className="text-muted-foreground">Delivered</dt>
                          <dd className="tnum text-foreground">{notifications.delivered}</dd>
                        </div>
                        <div>
                          <dt className="text-muted-foreground">Failed</dt>
                          <dd className="tnum text-foreground">{notifications.failed}</dd>
                        </div>
                      </dl>
                      <ul className="mt-1 flex flex-col">
                        {notifications.recent.slice(0, 10).map((notification) => (
                          <NotificationRow key={notification.id} notification={notification} />
                        ))}
                      </ul>
                    </>
                  )}
                </Panel>
              </div>

              {admin ? (
                <aside className="flex min-w-0 flex-col gap-5">
                  <Panel
                    title={editing === null ? "New rule" : "Edit rule"}
                    action={
                      editing === null ? null : (
                        <button
                          type="button"
                          onClick={() => setEditing(null)}
                          className="text-xs text-muted-foreground hover:text-foreground"
                        >
                          New instead
                        </button>
                      )
                    }
                  >
                    <AlertRuleForm
                      machines={machines}
                      rule={editing}
                      busy={busy}
                      onSave={saveRule}
                      onCancel={() => setEditing(null)}
                    />
                  </Panel>

                  <Panel title="Webhook">
                    <AlertWebhookPanel
                      status={data.webhook}
                      busy={busy}
                      onConfigure={configureWebhook}
                      onClear={clearWebhook}
                      onTest={testWebhook}
                    />
                  </Panel>
                </aside>
              ) : null}
            </div>
          )}
        </>
      )}
    </AppShell>
  );
}
