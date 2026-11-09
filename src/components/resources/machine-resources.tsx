"use client";

/**
 * Resource panels for the machine drilldown. Every value comes from the
 * deterministic `machineResourceView`; these components only choose layout
 * and wording. The panels cover total and per-core CPU, memory and swap,
 * GPU, disk and network detail, and every missing reading keeps its
 * explanatory state instead of becoming zero.
 */

import { Panel } from "@/components/panel";
import { MemoryUsage, Tag, UsageRing } from "@/components/live/gauge";
import type { MemoryFieldView, ScalarFieldView } from "@/live/presentation";
import { formatBytes, formatPercent } from "@/live/presentation";
import {
  formatCount,
  formatLatency,
  formatPower,
  formatRate,
  formatSpeedMbps,
  formatTemperature,
  type ByteUsage,
  type FamilyState,
  type MachineResourceView,
  type ResourceValue,
} from "@/resources/presentation";
import type { FilesystemView, GpuCardView, NetworkView, DiskView } from "@/resources/presentation";

import { Fact, Meter, Note, Subheading, UnavailableNote } from "./resource-primitives";

function scalarField(value: ResourceValue<number>): ScalarFieldView {
  if (value.state === "value") {
    return value.stale
      ? {out: "stale", value: value.value}
      : {out: "value", value: value.value};
  }
  if (value.state === "unavailable") return {out: "unavailable", reason: value.reason};
  return {out: "absent"};
}

function memoryField(value: ResourceValue<ByteUsage>): MemoryFieldView {
  if (value.state === "value") {
    return value.stale
      ? {out: "stale", usedBytes: value.value.usedBytes, totalBytes: value.value.totalBytes}
      : {out: "value", usedBytes: value.value.usedBytes, totalBytes: value.value.totalBytes};
  }
  if (value.state === "unavailable") return {out: "unavailable", reason: value.reason};
  return {out: "absent"};
}

function LastKnown({stale}: {stale: boolean}) {
  if (!stale) return null;
  return <Tag text="last known" title="Retained from the last accepted sample" />;
}

function percentOf(used: number, total: number): number | null {
  if (!Number.isFinite(used) || !Number.isFinite(total) || total <= 0) return null;
  return (used / total) * 100;
}

function familyNote(family: FamilyState, fallback: string): string {
  return family.state === "available" ? fallback : family.reason;
}

type Reading = {
  label: string;
  reading: ResourceValue<number>;
  format: (value: number) => string;
};

/**
 * Compact bottleneck readouts. Missing values show an em dash and group their
 * reason once, so an older collector that omits a field is never mistaken for
 * a successful zero.
 */
function ReadingGrid({
  readings,
  className = "",
}: {
  readings: readonly Reading[];
  className?: string;
}) {
  const reasonGroups = new Map<string, string[]>();
  for (const item of readings) {
    if (item.reading.state === "value") continue;
    const labels = reasonGroups.get(item.reading.reason) ?? [];
    labels.push(item.label);
    reasonGroups.set(item.reading.reason, labels);
  }
  return (
    <div className={className}>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 sm:grid-cols-4">
        {readings.map(({label, reading, format}) => {
          const shown = reading.state === "value";
          return (
            <div key={label} className="min-w-0">
              <dt className="text-xs text-muted-foreground">{label}</dt>
              <dd
                className="tnum text-xs text-foreground"
                title={shown ? undefined : reading.reason}
              >
                {shown ? format(reading.value) : "—"}
                {shown && reading.stale ? (
                  <span className="ml-1 text-muted-foreground">last known</span>
                ) : null}
              </dd>
            </div>
          );
        })}
      </dl>
      {reasonGroups.size > 0 ? (
        <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">
          {[...reasonGroups].map(([reason, labels]) => `${labels.join(", ")} — ${reason}`).join(" · ")}
        </p>
      ) : null}
    </div>
  );
}

export function CpuResourcePanel({
  view,
  ageLabel,
  held,
  className,
}: {
  view: MachineResourceView;
  ageLabel: string | null;
  held: boolean;
  className?: string;
}) {
  const cores = view.cpu.perCore;
  return (
    <Panel
      title="CPU"
      className={className}
      action={<LastKnown stale={view.stale} />}
    >
      <div className="grid gap-6 lg:grid-cols-[minmax(0,13rem)_minmax(0,1fr)] lg:items-start">
        <div className="flex flex-col items-center">
          <UsageRing
            metricName="CPU"
            field={scalarField(view.cpu.total)}
            ageLabel={ageLabel}
            isHeld={held}
            thresholdMs={view.thresholdMs}
          />
        </div>
        <div className="min-w-0">
          <div className="flex items-baseline justify-between gap-3">
            <Subheading>Per logical core</Subheading>
            {cores.state === "value" ? (
              <span className="tnum text-xs text-muted-foreground">
                {formatCount(cores.value.length)} cores
              </span>
            ) : null}
          </div>
          {cores.state === "value" ? (
            <>
              <ul className="mt-2 grid grid-cols-[repeat(auto-fit,minmax(3.5rem,1fr))] gap-1.5">
                {cores.value.map((usage, index) => {
                  const known = usage !== null;
                  const width = known ? Math.min(100, Math.max(0, usage)) : 0;
                  return (
                    <li
                      key={index}
                      className="rounded-md border border-border bg-card-strong/40 px-1.5 py-1"
                      title={
                        known
                          ? `Core ${index}: ${formatPercent(usage)}`
                          : `Core ${index}: counter regressed or was missing for this interval`
                      }
                    >
                      <div className="flex flex-col items-start gap-0.5">
                        <span className="tnum text-xs text-muted-foreground">C{index}</span>
                        <span className="tnum text-xs text-foreground">
                          {known ? formatPercent(usage) : "—"}
                        </span>
                      </div>
                      <div aria-hidden className="mt-1 h-0.5 w-full overflow-hidden rounded-full bg-border">
                        {known ? (
                          <div
                            className="h-full rounded-full"
                            style={{width: `${width}%`, backgroundColor: "var(--cpu)"}}
                          />
                        ) : null}
                      </div>
                      <span className="sr-only">
                        {known
                          ? `Core ${index} at ${formatPercent(usage)}`
                          : `Core ${index} unavailable this interval`}
                      </span>
                    </li>
                  );
                })}
              </ul>
            </>
          ) : cores.state === "unavailable" ? (
            <div className="mt-2">
              <UnavailableNote reason={cores.reason} />
            </div>
          ) : (
            <div className="mt-2">
              <Note>{cores.reason}</Note>
            </div>
          )}
        </div>
      </div>
      <div className="mt-4 border-t border-border pt-3">
        <Subheading>Heat, power and stalls</Subheading>
        <ReadingGrid
          className="mt-1"
          readings={[
            {
              label: "Temperature",
              reading: view.bottlenecks.cpuTemperature,
              format: formatTemperature,
            },
            {
              label: "Package power",
              reading: view.bottlenecks.cpuPower,
              format: formatPower,
            },
            {
              label: "I/O wait",
              reading: view.bottlenecks.cpuIowait,
              format: formatPercent,
            },
            {
              label: "Pressure",
              reading: view.bottlenecks.cpuPressure,
              format: formatPercent,
            },
          ]}
        />
      </div>
    </Panel>
  );
}

export function MemorySwapPanel({
  view,
  ageLabel,
  held,
  className,
}: {
  view: MachineResourceView;
  ageLabel: string | null;
  held: boolean;
  className?: string;
}) {
  const swap = view.swap;
  return (
    <Panel
      title="Memory and swap"
      className={className}
      action={<LastKnown stale={view.stale} />}
    >
      <div className="flex flex-col gap-5">
        <div>
          <Subheading>Memory</Subheading>
          <div className="mt-2">
            <MemoryUsage
              field={memoryField(view.memory)}
              ageLabel={ageLabel}
              isHeld={held}
              thresholdMs={view.thresholdMs}
            />
          </div>
        </div>
        <div className="border-t border-border pt-4">
          <Subheading>Pressure</Subheading>
          <ReadingGrid
            className="mt-1"
            readings={[
              {
                label: "Some tasks",
                reading: view.bottlenecks.memoryPressureSome,
                format: formatPercent,
              },
              {
                label: "Full",
                reading: view.bottlenecks.memoryPressureFull,
                format: formatPercent,
              },
            ]}
          />
        </div>
        <div className="border-t border-border pt-4">
          <Subheading>Swap</Subheading>
          {swap.state === "value" ? (
            swap.value.totalBytes === 0 ? (
              <div className="mt-2 flex flex-col gap-1">
                <p className="text-sm">No swap configured.</p>
                <Note>SwapTotal is zero; this is a valid configuration, not a missing reading.</Note>
              </div>
            ) : (
              <div className="mt-2 flex flex-col gap-2">
                <Meter
                  label="Used"
                  valueLabel={`${formatBytes(swap.value.usedBytes)} / ${formatBytes(swap.value.totalBytes)}`}
                  percent={percentOf(swap.value.usedBytes, swap.value.totalBytes)}
                  tone="swap"
                  ariaLabel={`Swap ${formatBytes(swap.value.usedBytes)} of ${formatBytes(swap.value.totalBytes)}`}
                />
                <p className="tnum text-xs text-muted-foreground">
                  {formatPercent(percentOf(swap.value.usedBytes, swap.value.totalBytes))} used
                  {swap.stale ? " · last known" : ""}
                </p>
              </div>
            )
          ) : swap.state === "unavailable" ? (
            <div className="mt-2">
              <UnavailableNote reason={swap.reason} />
            </div>
          ) : (
            <div className="mt-2">
              <Note>{swap.reason}</Note>
            </div>
          )}
        </div>
      </div>
    </Panel>
  );
}

function GpuCard({card}: {card: GpuCardView}) {
  const supported = card.telemetry === "supported";
  return (
    <article className="flex min-w-0 flex-col gap-3 rounded-lg border border-border bg-card-strong/25 p-3">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="truncate text-sm font-medium">{card.name}</h3>
          <p className="tnum truncate text-xs text-muted-foreground">
            {card.vendor}
            {card.pciBusId ? ` · ${card.pciBusId}` : ""}
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-1.5">
          {supported ? null : <Tag text="no telemetry" title={card.reason ?? "Management adapter"} />}
          {card.stale && card.state === "ok" ? <Tag text="last known" title="Retained from the last accepted sample" /> : null}
        </div>
      </div>

      {card.state === "ok" ? (
        <>
          <Meter
            label="GPU utilization"
            valueLabel={
              card.utilizationPercent === null
                ? "Not reported"
                : formatPercent(card.utilizationPercent)
            }
            percent={card.utilizationPercent}
            tone="gpu"
            ariaLabel={
              card.utilizationPercent === null
                ? `${card.name} utilization not reported`
                : `${card.name} utilization ${formatPercent(card.utilizationPercent)}`
            }
          />
          <Meter
            label="VRAM"
            valueLabel={
              card.memoryUsedBytes === null || card.memoryTotalBytes === null
                ? "Not reported"
                : `${formatBytes(card.memoryUsedBytes)} / ${formatBytes(card.memoryTotalBytes)}`
            }
            percent={
              card.memoryUsedBytes !== null && card.memoryTotalBytes !== null
                ? percentOf(card.memoryUsedBytes, card.memoryTotalBytes)
                : null
            }
            tone="gpu"
            ariaLabel={
              card.memoryUsedBytes === null || card.memoryTotalBytes === null
                ? `${card.name} VRAM not reported`
                : `${card.name} VRAM ${formatBytes(card.memoryUsedBytes)} of ${formatBytes(card.memoryTotalBytes)}`
            }
          />
          <dl className="grid grid-cols-2 gap-x-4 gap-y-1">
            <Fact
              label="Temperature"
              value={
                card.temperatureC === null
                  ? "Not reported"
                  : formatTemperature(card.temperatureC)
              }
            />
            <Fact
              label="Power"
              value={card.powerWatts === null ? "Not reported" : formatPower(card.powerWatts)}
            />
            <Fact
              label="Junction"
              value={
                card.junctionTemperatureC === null
                  ? "Not reported"
                  : formatTemperature(card.junctionTemperatureC)
              }
            />
            <Fact
              label="VRAM temp"
              value={
                card.vramTemperatureC === null
                  ? "Not reported"
                  : formatTemperature(card.vramTemperatureC)
              }
            />
          </dl>
        </>
      ) : (
        <div className="flex flex-col gap-1.5">
          <p className="text-xs text-muted-foreground">
            {card.state === "unsupported"
              ? "No telemetry."
              : "No readings for this device."}
          </p>
          {card.reason ? (
            <p className="text-xs text-warn">{card.reason}</p>
          ) : null}
          {card.memoryTotalBytes !== null ? (
            <dl className="mt-1">
              <Fact label="VRAM total" value={formatBytes(card.memoryTotalBytes)} />
            </dl>
          ) : null}
        </div>
      )}
    </article>
  );
}

export function GpuResourcePanel({
  view,
  className,
}: {
  view: MachineResourceView;
  className?: string;
}) {
  const cards = view.gpus.cards;
  return (
    <Panel
      title="GPUs"
      className={className}
      action={
        <span className="flex items-center gap-2">
          <span className="tnum">
            {cards.length === 0 ? "0 devices" : `${cards.length} device${cards.length === 1 ? "" : "s"}`}
          </span>
          <LastKnown stale={view.stale} />
        </span>
      }
    >
      {cards.length === 0 ? (
        view.gpus.family.state === "unavailable" ? (
          <UnavailableNote reason={view.gpus.family.reason} />
        ) : (
          <Note>{familyNote(view.gpus.family, "No GPU devices were reported.")}</Note>
        )
      ) : (
        <>
          <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
            {cards.map((card) => (
              <GpuCard key={card.id} card={card} />
            ))}
          </div>
          {view.gpus.family.state === "unavailable" ? (
            <p className="mt-3 text-xs text-warn">
              GPU unavailable · {view.gpus.family.reason}
            </p>
          ) : view.gpus.partial ? (
            <p className="mt-3 text-xs text-warn">
              Some GPUs are not reporting.
            </p>
          ) : null}
        </>
      )}
    </Panel>
  );
}

function FilesystemRow({filesystem}: {filesystem: FilesystemView}) {
  const percent =
    filesystem.state === "ok" &&
    filesystem.usedBytes !== null &&
    filesystem.totalBytes !== null
      ? percentOf(filesystem.usedBytes, filesystem.totalBytes)
      : null;
  const valueLabel =
    filesystem.state === "ok" &&
    filesystem.usedBytes !== null &&
    filesystem.totalBytes !== null
      ? `${formatBytes(filesystem.usedBytes)} / ${formatBytes(filesystem.totalBytes)}`
      : filesystem.totalBytes !== null
        ? `${formatBytes(filesystem.totalBytes)} total`
        : "Unavailable";
  const inodesPercent =
    filesystem.state === "ok" &&
    filesystem.inodesUsed !== null &&
    filesystem.inodesTotal !== null &&
    filesystem.inodesTotal > 0
      ? percentOf(filesystem.inodesUsed, filesystem.inodesTotal)
      : null;
  return (
    <li className="py-2.5">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <span className="break-all font-mono text-xs text-foreground">{filesystem.mountPoint}</span>
        <span className="tnum break-all text-xs text-muted-foreground">
          {filesystem.fsType}
          {filesystem.device ? ` · ${filesystem.device}` : ""}
        </span>
      </div>
      <div className="mt-2">
        <Meter
          label="Capacity"
          valueLabel={valueLabel}
          percent={percent}
          tone="disk"
          ariaLabel={`${filesystem.mountPoint} ${valueLabel}`}
        />
      </div>
      {filesystem.state === "ok" ? (
        <p
          className="tnum mt-1 text-xs text-muted-foreground"
          title={inodesPercent === null ? "Inode usage was not reported for this filesystem." : undefined}
        >
          {inodesPercent === null
            ? "Inodes not reported"
            : `Inodes ${formatPercent(inodesPercent)} used · ${formatCount(filesystem.inodesUsed)} / ${formatCount(filesystem.inodesTotal)}`}
        </p>
      ) : null}
      {filesystem.state === "unavailable" && filesystem.reason ? (
        <p className="mt-1.5 text-xs text-warn">{filesystem.reason}</p>
      ) : filesystem.state === "inventory" && filesystem.reason ? (
        <p className="mt-1.5 text-xs text-muted-foreground">{filesystem.reason}</p>
      ) : filesystem.stale ? (
        <p className="mt-1.5 text-xs text-muted-foreground">Last known usage.</p>
      ) : null}
    </li>
  );
}

function DiskRow({disk}: {disk: DiskView}) {
  const kind = disk.rotational === null ? "Unknown media" : disk.rotational ? "Rotational" : "Solid state";
  const optionalMissing = [
    disk.readLatencyMs === null ? "read latency" : null,
    disk.writeLatencyMs === null ? "write latency" : null,
    disk.ioUtilizationPercent === null ? "utilization" : null,
  ].filter((label): label is string => label !== null);
  return (
    <li className="py-2.5">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <span className="font-mono text-xs text-foreground">{disk.name}</span>
        <span className="tnum text-xs text-muted-foreground">
          {kind}
          {disk.sizeBytes !== null ? ` · ${formatBytes(disk.sizeBytes)}` : ""}
        </span>
      </div>
      {disk.model ? <p className="mt-0.5 truncate text-xs text-muted-foreground">{disk.model}</p> : null}
      {disk.state === "ok" ? (
        <>
          <dl className="mt-1.5 grid grid-cols-2 gap-x-4 gap-y-1 sm:grid-cols-4">
            <Fact label="Read" value={formatRate(disk.readBytesPerSec)} />
            <Fact label="Write" value={formatRate(disk.writeBytesPerSec)} />
            <Fact label="Read latency" value={formatLatency(disk.readLatencyMs)} />
            <Fact label="Write latency" value={formatLatency(disk.writeLatencyMs)} />
            <Fact
              label="Utilization"
              value={disk.ioUtilizationPercent === null ? "—" : formatPercent(disk.ioUtilizationPercent)}
            />
          </dl>
          {optionalMissing.length > 0 ? (
            <p className="mt-1 text-xs text-muted-foreground">
              Not reported: {optionalMissing.join(", ")}.
            </p>
          ) : null}
        </>
      ) : (
        <p className={`mt-1.5 text-xs ${disk.state === "unavailable" ? "text-warn" : "text-muted-foreground"}`}>
          {disk.reason ?? "No current I/O reading."}
        </p>
      )}
      {disk.state === "ok" && disk.stale ? (
        <p className="mt-1 text-xs text-muted-foreground">Last known rates.</p>
      ) : null}
    </li>
  );
}

export function DiskResourcePanel({
  view,
  className,
}: {
  view: MachineResourceView;
  className?: string;
}) {
  const filesystems = view.filesystems.items;
  const disks = view.disks.items;
  return (
    <Panel
      title="Disks"
      className={className}
      action={<LastKnown stale={view.stale} />}
    >
      {filesystems.length === 0 && disks.length === 0 ? (
        view.disks.family.state === "unavailable" ? (
          <UnavailableNote reason={view.disks.family.reason} />
        ) : (
          <Note>{familyNote(view.disks.family, "No disks or filesystems were reported.")}</Note>
        )
      ) : null}
      <div className="mt-5 flex flex-col gap-5">
        {filesystems.length > 0 ? (
          <div>
            <Subheading>Filesystems</Subheading>
            <ul className="mt-1 divide-y divide-border">
              {filesystems.map((filesystem) => (
                <FilesystemRow key={filesystem.id} filesystem={filesystem} />
              ))}
            </ul>
          </div>
        ) : null}
        {disks.length > 0 ? (
          <div className={filesystems.length > 0 ? "border-t border-border pt-4" : ""}>
            <Subheading>Devices</Subheading>
            <ul className="mt-1 divide-y divide-border">
              {disks.map((disk) => (
                <DiskRow key={disk.id} disk={disk} />
              ))}
            </ul>
          </div>
        ) : null}
        {view.disks.family.state !== "unavailable" ? (
          <div className={filesystems.length > 0 || disks.length > 0 ? "border-t border-border pt-4" : ""}>
            <Subheading>I/O pressure</Subheading>
            <ReadingGrid
              className="mt-1"
              readings={[
                {
                  label: "Some tasks",
                  reading: view.bottlenecks.ioPressureSome,
                  format: formatPercent,
                },
                {
                  label: "Full",
                  reading: view.bottlenecks.ioPressureFull,
                  format: formatPercent,
                },
              ]}
            />
          </div>
        ) : null}
      </div>
    </Panel>
  );
}

function NetworkRow({iface}: {iface: NetworkView}) {
  const stateLabel = iface.linkState === "up" ? "Link up" : iface.linkState === "down" ? "Link down" : "Link state unknown";
  return (
    <li className="py-3">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <span className="font-mono text-xs text-foreground">{iface.name}</span>
        <span className="flex flex-wrap items-center gap-1.5">
          {iface.virtual ? <Tag text="virtual" title="Virtual interface, not physical hardware" /> : null}
          <span className="tnum text-xs text-muted-foreground">{stateLabel}</span>
          {iface.speedMbps !== null ? (
            <span className="tnum text-xs text-muted-foreground">{formatSpeedMbps(iface.speedMbps)}</span>
          ) : null}
        </span>
      </div>
      {iface.addresses.length > 0 ? (
        <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
          {iface.addresses.map((address, index) => (
            <li
              key={`${address.family}:${address.address}/${address.prefix}:${index}`}
              className="tnum break-all font-mono text-xs text-muted-foreground"
            >
              {address.address}/{address.prefix}
              <span className="ml-1 font-sans text-xs uppercase tracking-wide">
                {address.family === "ipv4" ? "v4" : "v6"}
              </span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="mt-1 text-xs text-muted-foreground">No addresses reported.</p>
      )}
      {iface.state === "ok" ? (
        <dl className="mt-1.5 grid grid-cols-2 gap-x-4 gap-y-1">
          <Fact label="Receive" value={formatRate(iface.rxBytesPerSec)} />
          <Fact label="Transmit" value={formatRate(iface.txBytesPerSec)} />
        </dl>
      ) : (
        <p className={`mt-1.5 text-xs ${iface.state === "unavailable" ? "text-warn" : "text-muted-foreground"}`}>
          {iface.reason ?? "No current rate reading."}
        </p>
      )}
      {iface.state === "ok" && iface.stale ? (
        <p className="mt-1 text-xs text-muted-foreground">Last known rates.</p>
      ) : null}
    </li>
  );
}

export function NetworkResourcePanel({
  view,
  className,
}: {
  view: MachineResourceView;
  className?: string;
}) {
  const interfaces = view.network.items;
  return (
    <Panel
      title="Network"
      className={className}
      action={<LastKnown stale={view.stale} />}
    >
      {interfaces.length === 0 ? (
        view.network.family.state === "unavailable" ? (
          <UnavailableNote reason={view.network.family.reason} />
        ) : (
          <Note>{familyNote(view.network.family, "No network interfaces were reported.")}</Note>
        )
      ) : (
        <ul className="divide-y divide-border">
          {interfaces.map((iface) => (
            <NetworkRow key={iface.id} iface={iface} />
          ))}
        </ul>
      )}
    </Panel>
  );
}
