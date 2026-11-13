"use client";

/**
 * Connections surface: this hub's view of its collectors.
 *
 * Deliberately collector-facing only — destination URLs, tokens, headers and
 * raw transport diagnostics never reach this page. It shows connection state,
 * cadence, receipt freshness, delivery backlog/acknowledgment, replay and
 * drop counts, collection failures and capability inventory.
 */

import Link from "next/link";
import { Cable, Clock, Plus, ShieldCheck } from "lucide-react";

import { AppShell } from "@/components/shell/app-shell";
import { EmptyState, Panel } from "@/components/panel";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import { Tag } from "@/components/live/gauge";
import {
  StreamBanners,
  useAccessGate,
} from "@/components/live/stream-banners";
import { ageSince, formatAge, formatBytes, formatIntervalMs } from "@/live/presentation";
import { machineViewHref } from "@/live/machine-selection";
import { useLive } from "@/live/live-provider";
import { useNow } from "@/live/use-now";
import {
  collectorConnectionViews,
  type CapabilityView,
  type CollectorConnectionView,
} from "@/resources/presentation";

function formatWhen(iso: string | null): string {
  if (iso === null) return "—";
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return iso;
  return date.toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "medium",
  });
}

function statusTone(status: CapabilityView["status"]): string {
  if (status === "supported") return "bg-ok-soft text-ok";
  if (status === "unsupported") return "bg-warn-soft text-warn";
  return "border border-border text-muted-foreground";
}

function CapabilityBadge({ capability }: { capability: CapabilityView }) {
  return (
    <li className="rounded-md border border-border bg-card-strong/30 px-2 py-1.5">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs text-foreground">{capability.label}</span>
        <span className={`tnum rounded-full px-1.5 py-0.5 text-xs ${statusTone(capability.status)}`}>
          {capability.status}
        </span>
      </div>
      {capability.reason ? (
        <p className="mt-1 text-xs text-muted-foreground">{capability.reason}</p>
      ) : null}
    </li>
  );
}

function ConnectionHeader({ view, now }: { view: CollectorConnectionView; now: number }) {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
      <span className="tnum inline-flex items-center gap-1.5 text-sm">
        <span
          aria-hidden
          className={`inline-block h-2 w-2 shrink-0 rounded-full ${view.connected ? "bg-ok" : "bg-crit"}`}
        />
        {view.connected ? "Connected" : "Disconnected"}
      </span>
      <span className="tnum text-xs text-muted-foreground">
        Interval {view.cadenceMs === null ? "unknown" : formatIntervalMs(view.cadenceMs)}
      </span>
      <span className="tnum text-xs text-muted-foreground">
        Updated {formatAge(ageSince(view.lastReceivedAt, now))}
      </span>
      {view.freshness === "stale" ? (
        <Tag text="stale" title={`No update for over ${formatIntervalMs(view.thresholdMs)}`} />
      ) : view.freshness === "unknown" ? (
        <Tag text="no sample" title="No receipt timestamp is available yet" />
      ) : null}
    </div>
  );
}

function DeliveryFacts({ view }: { view: CollectorConnectionView }) {
  if (view.delivery === null) {
    return (
      <p className="text-xs text-muted-foreground">
        Delivery status unavailable.
      </p>
    );
  }
  const delivery = view.delivery;
  return (
    <dl className="grid gap-x-6 gap-y-2 sm:grid-cols-2">
      {delivery.queueMode === undefined ? null : (
        <div className="flex items-baseline justify-between gap-3">
          <dt className="text-xs text-muted-foreground">Backlog stored</dt>
          <dd className="text-xs text-foreground">
            {delivery.queueMode === "durable" ? "On disk, survives a restart" : "In RAM, lost on a restart"}
          </dd>
        </div>
      )}
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">Backlog records</dt>
        <dd className="tnum text-xs text-foreground">{delivery.pendingRecords}</dd>
      </div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">Backlog bytes</dt>
        <dd className="tnum text-xs text-foreground">{formatBytes(delivery.pendingBytes)}</dd>
      </div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">In-flight records</dt>
        <dd className="tnum text-xs text-foreground">{delivery.inFlightRecords}</dd>
      </div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">Oldest pending</dt>
        <dd className="tnum text-right text-xs text-foreground">
          {formatWhen(delivery.oldestPendingCapturedAt)}
        </dd>
      </div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">Last acknowledgment</dt>
        <dd className="tnum text-right text-xs text-foreground">
          {delivery.lastAckAt === null ? "No acknowledgment yet" : formatWhen(delivery.lastAckAt)}
        </dd>
      </div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">Dropped / gap records</dt>
        <dd className="tnum text-xs text-foreground">{delivery.droppedRecords}</dd>
      </div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">Replaying</dt>
        <dd className="tnum text-xs text-foreground">{delivery.replaying ? "Yes" : "No"}</dd>
      </div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-xs text-muted-foreground">Status captured</dt>
        <dd className="tnum text-right text-xs text-foreground">
          {formatWhen(view.deliveryCapturedAt)}
        </dd>
      </div>
    </dl>
  );
}

function CollectorPanel({ view, now }: { view: CollectorConnectionView; now: number }) {
  return (
    <Panel
      title={view.name}
      action={
        <Link
          href={machineViewHref(view.machineId, "overview")}
          className="text-xs text-accent hover:underline"
        >
          Open machine
        </Link>
      }
    >
      <div className="flex flex-col gap-5">
        <ConnectionHeader view={view} now={now} />

        <section aria-label={`${view.name} delivery`}>
          <h3 className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
            <Clock className="h-3.5 w-3.5" aria-hidden />
            Delivery
          </h3>
          <div className="mt-2">
            <DeliveryFacts view={view} />
          </div>
        </section>

        <section aria-label={`${view.name} collection failures`} className="border-t border-border pt-4">
          <h3 className="text-xs font-medium text-muted-foreground">Collection failures</h3>
          {view.delivery === null ? (
            <p className="mt-2 text-xs text-muted-foreground">
              Failure status unavailable.
            </p>
          ) : view.failures.length === 0 ? (
            <p className="mt-2 text-xs text-muted-foreground">None</p>
          ) : (
            <ul className="mt-2 flex flex-col gap-1.5">
              {view.failures.map((failure, index) => (
                <li key={`${failure.metric}\u0000${failure.resourceId ?? ""}\u0000${failure.reason}\u0000${index}`} className="text-xs">
                  <span className="text-foreground">{failure.metric}</span>
                  {failure.resourceId ? (
                    <span className="tnum font-mono text-xs text-muted-foreground"> · {failure.resourceId}</span>
                  ) : null}
                  <span className="text-muted-foreground"> — {failure.reason}</span>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section aria-label={`${view.name} capabilities`} className="border-t border-border pt-4">
          <h3 className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
            <ShieldCheck className="h-3.5 w-3.5" aria-hidden />
            Capabilities
          </h3>
          {view.capabilities === null ? (
            <p className="mt-2 text-xs text-muted-foreground">
              Capabilities unavailable.
            </p>
          ) : (
            <ul className="mt-2 grid gap-1.5 sm:grid-cols-2 xl:grid-cols-3">
              {view.capabilities.map((capability) => (
                <CapabilityBadge key={capability.key} capability={capability} />
              ))}
            </ul>
          )}
        </section>
      </div>
    </Panel>
  );
}

export function ConnectionsView() {
  const { phase, machines, session } = useLive();
  const gate = useAccessGate();
  const now = useNow();
  const held = phase === "reconnecting" || phase === "disconnected";
  const views = collectorConnectionViews(machines, now);

  return (
    <AppShell
      title="Connections"
      headerRight={
        <>
          {session.kind === "signed-in" && session.user.admin ? (
            <Link href="/administration#collectors" className="inline-flex items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs hover:bg-card-strong">
              <Plus className="h-3.5 w-3.5" aria-hidden />Add machine
            </Link>
          ) : null}
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

          {(phase === "live" || held) && machines.length === 0 ? (
            <Panel title="No collectors" className="mt-8">
              <EmptyState
                icon={Cable}
                note="No collectors are connected to this hub in your authorized scope."
              />
            </Panel>
          ) : null}

          {views.length === 0 && phase === "connecting" ? (
            <Panel title="Waiting for collector status" className="mt-8">
              <EmptyState
                icon={Cable}
                note="Waiting for the first snapshot…"
              />
            </Panel>
          ) : null}

          {views.length > 0 ? (
            <>
              <h2 className="mb-4 text-sm font-medium tracking-tight">Collectors</h2>
              {/* auto-fit: a lone collector card fills the row instead of stranding it in an empty second column. */}
              <div className="grid gap-5 xl:grid-cols-[repeat(auto-fit,minmax(30rem,1fr))]">
                {views.map((view) => (
                  <CollectorPanel key={view.machineId} view={view} now={now} />
                ))}
              </div>
            </>
          ) : null}
        </>
      )}
    </AppShell>
  );
}
