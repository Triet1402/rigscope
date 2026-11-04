/**
 * Metric readout primitives shared by the fleet table and machine views.
 * Presentational only (no hooks, no provider) so they render anywhere.
 *
 * Honesty rules: an unavailable metric shows its reason, never a zero; a
 * stale metric keeps its last-known value with an explicit label; "held"
 * marks values kept while the stream is down.
 */

import {
  FRESHNESS_THRESHOLD_MS,
  formatBytes,
  formatIntervalMs,
  formatPercent,
  type MemoryFieldView,
  type ScalarFieldView,
} from "@/live/presentation";

const RING_SIZE = 160;
const RING_RADIUS = 70;
const RING_CIRCUMFERENCE = 2 * Math.PI * RING_RADIUS;

/** Receipt-age copy that states the threshold actually applied to the machine. */
function staleCopy(thresholdMs: number | null | undefined): string {
  return `no update for over ${formatIntervalMs(thresholdMs ?? FRESHNESS_THRESHOLD_MS)}`;
}

function clampPercent(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.min(100, Math.max(0, value));
}

function StateText(props: { reason: string; ageLabel: string | null; center?: boolean }) {
  const { reason, ageLabel, center } = props;
  return (
    <p className={center ? "max-w-[16rem] text-center text-xs text-warn" : "text-xs text-warn"}>
      Unavailable — {reason}
      {ageLabel ? (
        <span className="text-muted-foreground"> (last update {ageLabel})</span>
      ) : null}
    </p>
  );
}

/** Dashed placeholder ring for an unavailable/absent metric: an explicit —. */
function UnavailableRing(props: { reason: string; ageLabel: string | null }) {
  const { reason, ageLabel } = props;
  return (
    <div className="flex flex-col items-center gap-2">
      <div
        role="img"
        aria-label={`Unavailable — ${reason}`}
        className="flex h-40 w-40 shrink-0 items-center justify-center rounded-full border-[5px] border-dashed border-border"
      >
        <span className="tnum text-xl font-semibold text-muted-foreground">—</span>
      </div>
      <StateText reason={reason} ageLabel={ageLabel} center />
    </div>
  );
}

// ---------------------------------------------------------------------------
// CPU-style 0..100% ring
// ---------------------------------------------------------------------------

/**
 * Ring readout for a scalar percent. Values are presented whether the way
 * `field.out` says: value (live), stale (retained last-known), unavailable
 * (reason), absent (no sample yet).
 */
export function UsageRing(props: {
  metricName: string;
  field: ScalarFieldView;
  ageLabel?: string | null;
  isHeld?: boolean;
  /** The machine's resolved staleness threshold; falls back to 15 s. */
  thresholdMs?: number;
}) {
  const { metricName, field, ageLabel, isHeld, thresholdMs } = props;

  if (field.out === "unavailable") {
    return <UnavailableRing reason={field.reason} ageLabel={ageLabel ?? null} />;
  }
  if (field.out === "absent") {
    return (
      <UnavailableRing
        reason="No sample has arrived yet."
        ageLabel={ageLabel ?? null}
      />
    );
  }

  const stale = field.out === "stale";
  const percent = clampPercent(field.value);
  const stroke = stale ? "var(--warn)" : "var(--cpu)";
  const dashOffset = RING_CIRCUMFERENCE * (1 - percent / 100);
  const ariaLabel = stale
    ? `${metricName} last known ${formatPercent(percent)}, stale`
    : `${metricName} ${formatPercent(percent)}`;

  return (
    <div className="flex flex-col items-center gap-3">
      <svg
        role="img"
        aria-label={ariaLabel}
        viewBox={`0 0 ${RING_SIZE} ${RING_SIZE}`}
        className="h-40 w-40 shrink-0"
      >
        <circle
          cx={RING_SIZE / 2}
          cy={RING_SIZE / 2}
          r={RING_RADIUS}
          fill="none"
          stroke="var(--border)"
          strokeWidth={8}
        />
        <circle
          cx={RING_SIZE / 2}
          cy={RING_SIZE / 2}
          r={RING_RADIUS}
          fill="none"
          stroke={stroke}
          strokeWidth={8}
          strokeLinecap="round"
          strokeDasharray={RING_CIRCUMFERENCE}
          strokeDashoffset={dashOffset}
          transform={`rotate(-90 ${RING_SIZE / 2} ${RING_SIZE / 2})`}
        />
        <text
          x="50%"
          y={stale || isHeld ? "45%" : "50%"}
          dominantBaseline="central"
          textAnchor="middle"
          className="fill-foreground text-xl font-semibold tnum"
        >
          {formatPercent(percent)}
        </text>
        {stale ? (
          <text
            x="50%"
            y={RING_SIZE / 2 + 22}
            textAnchor="middle"
            className="fill-warn text-xs tnum"
          >
            last known
          </text>
        ) : isHeld ? (
          <text
            x="50%"
            y={RING_SIZE / 2 + 22}
            textAnchor="middle"
            className="fill-muted-foreground text-xs tnum"
          >
            held
          </text>
        ) : null}
      </svg>
      <p className="text-center text-xs text-muted-foreground">
        {stale
          ? `Last known value — ${staleCopy(thresholdMs)}${ageLabel ? ` (${ageLabel})` : ""}`
          : (ageLabel ?? "")}
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Memory-style used/total bar
// ---------------------------------------------------------------------------

function MemoryUnavailable(props: { reason: string; ageLabel: string | null }) {
  const { reason, ageLabel } = props;
  return (
    <div className="flex flex-col gap-1.5">
      <div
        role="img"
        aria-label={`Memory unavailable — ${reason}`}
        className="h-3 w-full max-w-64 rounded-full border border-dashed border-border"
      />
      <StateText reason={reason} ageLabel={ageLabel} />
    </div>
  );
}

/** Horizontal used/total bar. Bytes shape is kept; percentages derive from bytes. */
export function MemoryUsage(props: {
  field: MemoryFieldView;
  ageLabel?: string | null;
  isHeld?: boolean;
  /** The machine's resolved staleness threshold; falls back to 15 s. */
  thresholdMs?: number;
}) {
  const { field, ageLabel, isHeld, thresholdMs } = props;

  if (field.out === "unavailable") {
    return <MemoryUnavailable reason={field.reason} ageLabel={ageLabel ?? null} />;
  }
  if (field.out === "absent") {
    return (
      <MemoryUnavailable reason="No sample has arrived yet." ageLabel={ageLabel ?? null} />
    );
  }

  const stale = field.out === "stale";
  const total = field.totalBytes;
  const used = field.usedBytes;
  const fraction = total > 0 ? clampPercent((used / total) * 100) / 100 : 0;
  const fill = stale ? "var(--warn)" : "var(--mem)";
  const ariaLabel = stale
    ? `Memory last known ${formatBytes(used)} of ${formatBytes(total)}, stale`
    : `Memory ${formatBytes(used)} of ${formatBytes(total)}`;

  return (
    <div className="flex flex-col gap-2">
      <div
        role="img"
        aria-label={ariaLabel}
        className="flex h-3 w-full max-w-64 items-stretch overflow-hidden rounded-full bg-card-strong"
      >
        <div
          className="h-full rounded-full"
          style={{
            width: `${fraction * 100}%`,
            backgroundColor: fill,
            minWidth: total > 0 ? undefined : 0,
          }}
        />
      </div>
      <div className="flex items-baseline gap-2">
        <p className="tnum text-sm">
          {total > 0 ? (
            <>
              <span className="font-semibold">{formatBytes(used)}</span>
              <span className="text-muted-foreground"> of {formatBytes(total)}</span>
              <span className="text-muted-foreground">
                {" "}
                · {formatPercent(total > 0 ? (used / total) * 100 : null)}
              </span>
            </>
          ) : (
            <span className="text-muted-foreground">
              {formatBytes(used)} used — total size unavailable
            </span>
          )}
        </p>
        {stale ? <Tag text="last known" title={`No update for over ${formatIntervalMs(thresholdMs ?? FRESHNESS_THRESHOLD_MS)}`} /> : null}
        {isHeld && !stale ? <Tag text="held" title="Value kept while the connection is down" /> : null}
      </div>
      <p className="text-xs text-muted-foreground">
        {stale
          ? `Retained from the last accepted sample${ageLabel ? ` (${ageLabel})` : ""}.`
          : (ageLabel ?? "")}
      </p>
    </div>
  );
}

export function Tag(props: { text: string; title: string }) {
  const { text, title } = props;
  return (
    <span
      title={title}
      className="tnum shrink-0 rounded-full border border-border px-2 py-0.5 text-xs text-muted-foreground"
    >
      {text}
    </span>
  );
}
