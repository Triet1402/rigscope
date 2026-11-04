/** Presentation helpers shared by the alerts UI (browser-safe, no Node). */
import {
  ALERT_CONDITIONS,
  ALERT_METRICS,
  alertMetricUnit,
  type AlertCondition,
  type AlertMetric,
  type AlertMetricUnit,
} from "./types";

export function formatBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = Math.max(0, bytes);
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return index === 0 ? `${Math.round(value)} ${units[index]}` : `${value.toFixed(1)} ${units[index]}`;
}

export function formatMetricValue(metric: AlertMetric, value: number | null): string {
  if (value === null || !Number.isFinite(value)) return "—";
  const unit: AlertMetricUnit = alertMetricUnit(metric);
  switch (unit) {
    case "percent":
      return `${Math.round(value * 100) / 100}%`;
    case "bytes":
      return formatBytes(value);
    case "bytes_per_second":
      return `${formatBytes(value)}/s`;
    case "celsius":
      return `${Math.round(value)} °C`;
    case "watts":
      return `${Math.round(value)} W`;
    case "milliseconds":
      return `${Math.round(value * 10) / 10} ms`;
  }
}

export function formatAlertDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "Immediately";
  if (ms < 60_000) return `${Math.round(ms / 1000)} s`;
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)} min`;
  return `${Math.round(ms / 3_600_000)} h`;
}

export function alertConditionLabel(condition: AlertCondition): string {
  return ALERT_CONDITIONS.find((item) => item.value === condition)?.label ?? condition;
}

export function alertConditionNeedsThreshold(condition: AlertCondition): boolean {
  return ALERT_CONDITIONS.find((item) => item.value === condition)?.needsThreshold ?? false;
}

export function alertMetricLabel(metric: AlertMetric, resourceId: string): string {
  return `${ALERT_METRICS[metric].label}${resourceId ? ` (${resourceId})` : ""}`;
}

/** ISO timestamp → value for a native `<input type="datetime-local">`. */
export function toDateTimeLocal(iso: string | null): string {
  if (iso === null) return "";
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return "";
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function fromDateTimeLocal(value: string): string | null {
  if (value.trim() === "") return null;
  const parsed = new Date(value);
  return Number.isFinite(parsed.getTime()) ? parsed.toISOString() : null;
}
