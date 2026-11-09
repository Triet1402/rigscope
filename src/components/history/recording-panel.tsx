"use client";

/**
 * Explicit recording controls for History.
 *
 * The panel always states whether charts are backed by temporary memory or
 * opt-in persistent history, then offers a bounded server-side capture. The
 * browser downloads an opaque recording id; it never supplies a filesystem
 * path.
 */

import { useEffect, useMemo, useState } from "react";
import { CircleStop, Download, FileClock, Play, RefreshCw, Trash2 } from "lucide-react";

import type { MachineState } from "@/contracts";
import type { HistoryStatus } from "@/hub/storage/history";
import type {
  RecordingRecord,
  RecordingResolution,
  RecordingStatus,
} from "@/hub/recordings/manager";

const RESOLUTION_OPTIONS: Array<{ id: RecordingResolution; label: string }> = [
  { id: "1s", label: "1 second" },
  { id: "10s", label: "10 seconds" },
  { id: "1m", label: "1 minute" },
  { id: "5m", label: "5 minutes" },
  { id: "1h", label: "1 hour" },
];

const STATUS_TEXT: Record<RecordingStatus, string> = {
  recording: "Recording",
  stopped: "Stopped",
  completed: "Completed",
  interrupted: "Interrupted by restart",
  failed: "Failed",
};

function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(1)} KiB`;
  return `${(value / 1024 ** 2).toFixed(1)} MiB`;
}

function formatWhen(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function sizeLimitLabel(status: HistoryStatus | null): string {
  if (status === null) return "History mode unavailable";
  if (status.mode === "memory") {
    const used = status.memory?.usedBytes ?? 0;
    const budget = status.memory?.budgetBytes ?? 0;
    return `Temporary RAM history · ${formatBytes(used)} of ${formatBytes(budget)} used`;
  }
  const tier = status.persistence?.storageTier === "summary" ? "compact 10s summaries" : "raw samples";
  return `Persistent history · ${tier} · flush ${((status.persistence?.flushIntervalMs ?? 10_000) / 1000).toFixed(0)}s`;
}

export function RecordingPanel({
  machine,
  selectedMetrics,
  csrfToken,
}: {
  machine: MachineState | undefined;
  selectedMetrics: readonly string[];
  csrfToken: string | null;
}) {
  const [status, setStatus] = useState<HistoryStatus | null>(null);
  const [recordings, setRecordings] = useState<RecordingRecord[]>([]);
  const [open, setOpen] = useState(false);
  const [resolution, setResolution] = useState<RecordingResolution>("10s");
  const [minutes, setMinutes] = useState(15);
  const [includeBacklog, setIncludeBacklog] = useState(true);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const machineId = machine?.machineId ?? "";

  const refresh = async (): Promise<void> => {
    try {
      const [statusResponse, listResponse] = await Promise.all([
        fetch("/api/history/status", { cache: "no-store", headers: { Accept: "application/json" } }),
        fetch("/api/recordings", { cache: "no-store", headers: { Accept: "application/json" } }),
      ]);
      if (statusResponse.ok) setStatus((await statusResponse.json()) as HistoryStatus);
      if (listResponse.ok) {
        const body = (await listResponse.json()) as { recordings?: RecordingRecord[] };
        setRecordings(Array.isArray(body.recordings) ? body.recordings : []);
      }
    } catch {
      // A transient failure leaves the last known state visible.
    }
  };

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void refresh();
    }, 5_000);
    return () => window.clearInterval(timer);
  }, [machineId, csrfToken]);

  const visible = useMemo(
    () => recordings.filter((recording) => recording.machines.includes(machineId)).slice(0, 8),
    [recordings, machineId],
  );
  const active = visible.find((recording) => recording.status === "recording") ?? null;
  const disabled = machine === undefined || csrfToken === null || selectedMetrics.length === 0 || busy;

  const mutate = async (path: string, init: RequestInit): Promise<void> => {
    if (csrfToken === null) return;
    setBusy(true);
    setMessage(null);
    try {
      const response = await fetch(path, {
        ...init,
        headers: {
          ...(init.body === undefined ? {} : { "Content-Type": "application/json" }),
          "x-csrf-token": csrfToken,
          Accept: "application/json",
        },
      });
      if (!response.ok) {
        const body = (await response.json().catch(() => ({}))) as { error?: string };
        setMessage(body.error ?? `Request failed (${response.status}).`);
        return;
      }
      setOpen(false);
      await refresh();
    } catch {
      setMessage("Could not reach the hub. Try again.");
    } finally {
      setBusy(false);
    }
  };

  const start = async (): Promise<void> => {
    if (machine === undefined) return;
    await mutate("/api/recordings", {
      method: "POST",
      body: JSON.stringify({
        machines: [machine.machineId],
        metrics: selectedMetrics,
        resolution,
        durationMs: Math.max(1, Math.round(minutes)) * 60_000,
        includeBacklog,
      }),
    });
  };

  const stop = async (id: string): Promise<void> => {
    await mutate(`/api/recordings/${id}/stop`, { method: "POST", body: "{}" });
  };

  const remove = async (id: string): Promise<void> => {
    await mutate(`/api/recordings/${id}`, { method: "DELETE" });
  };

  return (
    <section
      aria-labelledby="recording-heading"
      className="mb-5 rounded-xl border border-border bg-card p-4 md:p-5"
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 id="recording-heading" className="flex items-center gap-2 text-sm font-medium text-foreground">
            <FileClock className="h-4 w-4 text-muted-foreground" aria-hidden />
            Recording
          </h2>
          <p className="mt-1 text-xs text-muted-foreground">{sizeLimitLabel(status)}</p>
          {status?.mode === "memory" ? (
            <p className="mt-1 text-xs text-muted-foreground">
              Charts and recent history are temporary and disappear when the hub restarts. Start a recording to keep
              a bounded snapshot on this hub.
            </p>
          ) : (
            <p className="mt-1 text-xs text-muted-foreground">
              Charts use opt-in persistent summaries. A recording keeps an explicit, downloadable capture.
            </p>
          )}
        </div>
        {active === null ? (
          <button
            type="button"
            onClick={() => setOpen((value) => !value)}
            disabled={disabled}
            className="inline-flex min-h-9 items-center gap-1.5 rounded-md border border-border px-3 text-xs font-medium text-foreground transition-colors hover:bg-card-strong disabled:cursor-not-allowed disabled:opacity-50"
          >
            <Play className="h-3.5 w-3.5" aria-hidden />
            Start recording
          </button>
        ) : (
          <button
            type="button"
            onClick={() => void stop(active.id)}
            disabled={busy}
            className="inline-flex min-h-9 items-center gap-1.5 rounded-md border border-border px-3 text-xs font-medium text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
          >
            <CircleStop className="h-3.5 w-3.5" aria-hidden />
            Stop recording
          </button>
        )}
      </div>

      {open && active === null ? (
        <form
          className="mt-4 grid gap-3 border-t border-border pt-4 sm:grid-cols-2 lg:grid-cols-4"
          onSubmit={(event) => {
            event.preventDefault();
            void start();
          }}
        >
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            Resolution
            <select
              value={resolution}
              onChange={(event) => setResolution(event.target.value as RecordingResolution)}
              className="min-h-9 rounded-md border border-border bg-background px-2 text-xs text-foreground"
            >
              {RESOLUTION_OPTIONS.map((option) => (
                <option key={option.id} value={option.id}>{option.label}</option>
              ))}
            </select>
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            Duration (minutes)
            <input
              type="number"
              min={1}
              max={1440}
              value={minutes}
              onChange={(event) => setMinutes(Number(event.target.value))}
              className="min-h-9 rounded-md border border-border bg-background px-2 text-xs text-foreground"
            />
          </label>
          <label className="flex min-h-9 items-end gap-2 pb-1 text-xs text-foreground">
            <input
              type="checkbox"
              checked={includeBacklog}
              onChange={(event) => setIncludeBacklog(event.target.checked)}
              className="h-4 w-4 accent-[var(--accent)]"
            />
            Include recent RAM history
          </label>
          <div className="flex items-end">
            <button
              type="submit"
              disabled={disabled}
              className="inline-flex min-h-9 w-full items-center justify-center gap-1.5 rounded-md bg-accent px-3 text-xs font-medium text-accent-foreground transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <Play className="h-3.5 w-3.5" aria-hidden />
              {busy ? "Starting…" : "Start capture"}
            </button>
          </div>
          <p className="text-xs text-muted-foreground sm:col-span-2 lg:col-span-4">
            {selectedMetrics.length} selected metric{selectedMetrics.length === 1 ? "" : "s"} will be written to a
            private file on the hub. Downloads are available only to your account.
          </p>
        </form>
      ) : null}

      {message !== null ? (
        <p role="status" className="mt-3 text-xs text-warn">{message}</p>
      ) : null}

      {visible.length > 0 ? (
        <ul className="mt-4 flex flex-col gap-2 border-t border-border pt-3">
          {visible.map((recording) => (
            <li key={recording.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
              <span className={recording.status === "recording" ? "text-ok" : "text-foreground"}>
                {STATUS_TEXT[recording.status]}
              </span>
              <span className="text-muted-foreground">{recording.resolution} · {formatWhen(recording.startedAt)}</span>
              <span className="tnum text-muted-foreground">{formatBytes(recording.bytesWritten)}</span>
              {recording.reason !== null ? (
                <span className="text-muted-foreground">· {recording.reason}</span>
              ) : null}
              <span className="ml-auto flex items-center gap-1">
                <a
                  href={recording.downloadPath}
                  className="inline-flex min-h-8 items-center gap-1 rounded-md border border-border px-2 text-xs text-foreground transition-colors hover:bg-card-strong"
                  aria-label={`Download recording from ${formatWhen(recording.startedAt)}`}
                >
                  <Download className="h-3 w-3" aria-hidden />
                  Download
                </a>
                <button
                  type="button"
                  onClick={() => void remove(recording.id)}
                  disabled={busy || recording.status === "recording"}
                  className="inline-flex min-h-8 items-center gap-1 rounded-md border border-border px-2 text-xs text-muted-foreground transition-colors hover:bg-card-strong hover:text-crit disabled:opacity-40"
                  aria-label={`Delete recording from ${formatWhen(recording.startedAt)}`}
                >
                  <Trash2 className="h-3 w-3" aria-hidden />
                  Delete
                </button>
              </span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="mt-3 flex items-center gap-1.5 text-xs text-muted-foreground">
          <RefreshCw className="h-3 w-3" aria-hidden />
          No recordings for this machine yet.
        </p>
      )}
    </section>
  );
}
