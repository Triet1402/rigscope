"use client";

/** Small, explicit rule form for the Alerts surface. */
import { useEffect, useState } from "react";

import {
  ALERT_CONDITIONS,
  ALERT_METRICS,
  ALERT_LIMITS,
  alertMetricNeedsResource,
  alertMetricUnit,
  type AlertCondition,
  type AlertMetric,
  type AlertRule,
  type AlertRuleInput,
} from "@/alerts/types";
import { fromDateTimeLocal, toDateTimeLocal } from "@/alerts/format";
import { resolveAlertPresets } from "@/alerts/presets";
import type { MachineState } from "@/contracts";

type ResourceOption = {value: string; label: string};

function metricOptions() {
  return (Object.keys(ALERT_METRICS) as AlertMetric[]).map((metric) => ({
    value: metric,
    label: ALERT_METRICS[metric].label,
  }));
}

function resourceOptions(metric: AlertMetric, machine: MachineState | undefined): ResourceOption[] {
  if (!machine || !alertMetricNeedsResource(metric)) return [];
  const inventory = machine.inventory;
  if (inventory === undefined) return [];
  if (metric === "cpu.core") {
    return Array.from({length: Math.min(inventory.cpu.logicalCores, 256)}, (_value, index) => ({
      value: `core:${index}`,
      label: `Core ${index}`,
    }));
  }
  if (metric.startsWith("gpu.")) {
    return inventory.gpus
      .filter((gpu) => gpu.telemetry === "supported")
      .map((gpu) => ({value: gpu.id, label: gpu.name}));
  }
  if (metric.startsWith("fs.")) {
    return inventory.filesystems.map((filesystem) => ({
      value: filesystem.id,
      label: `${filesystem.mountPoint} (${filesystem.fsType})`,
    }));
  }
  if (metric.startsWith("disk.")) {
    return inventory.disks.map((disk) => ({value: disk.id, label: disk.name}));
  }
  if (metric.startsWith("net.")) {
    return inventory.network.map((iface) => ({value: iface.id, label: iface.name}));
  }
  return [];
}

function placeholderFor(metric: AlertMetric): string {
  if (metric === "cpu.core") return "core:0";
  if (metric.startsWith("gpu.")) return "gpu:GPU-…";
  if (metric.startsWith("fs.")) return "fs:/";
  if (metric.startsWith("disk.")) return "disk:sda";
  if (metric.startsWith("net.")) return "net:eno1";
  return "Resource id";
}

function unitHint(metric: AlertMetric): string {
  switch (alertMetricUnit(metric)) {
    case "percent":
      return "percent (0–100)";
    case "bytes":
      return "bytes";
    case "bytes_per_second":
      return "bytes per second";
    case "celsius":
      return "degrees Celsius";
    case "watts":
      return "watts";
    case "milliseconds":
      return "milliseconds";
  }
}

export function AlertRuleForm({
  machines,
  rule,
  busy,
  onSave,
  onCancel,
}: {
  machines: readonly MachineState[];
  rule: AlertRule | null;
  busy: boolean;
  onSave: (input: AlertRuleInput) => Promise<void>;
  onCancel: () => void;
}) {
  const defaultMachine = machines[0]?.machineId ?? "";
  const [name, setName] = useState("");
  const [machineId, setMachineId] = useState(defaultMachine);
  const [metric, setMetric] = useState<AlertMetric>("cpu.usage");
  const [resourceId, setResourceId] = useState("");
  const [condition, setCondition] = useState<AlertCondition>("above");
  const [threshold, setThreshold] = useState("80");
  const [durationSeconds, setDurationSeconds] = useState("0");
  const [enabled, setEnabled] = useState(true);
  const [maintenanceLocal, setMaintenanceLocal] = useState("");
  const [presetId, setPresetId] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (rule === null) {
      setName("");
      setMachineId(machines[0]?.machineId ?? "");
      setMetric("cpu.usage");
      setResourceId("");
      setCondition("above");
      setThreshold("80");
      setDurationSeconds("0");
      setEnabled(true);
      setMaintenanceLocal("");
      setPresetId("");
      setError(null);
      return;
    }
    setName(rule.name);
    setMachineId(rule.machineId);
    setMetric(rule.metric);
    setResourceId(rule.resourceId);
    setCondition(rule.condition);
    setThreshold(rule.threshold === null ? "" : String(rule.threshold));
    setDurationSeconds(String(rule.durationMs / 1000));
    setEnabled(rule.enabled);
    setMaintenanceLocal(toDateTimeLocal(rule.maintenanceUntil));
    setPresetId("");
    setError(null);
    // machines is stable for the form's lifetime; rule changes reset the form.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rule]);

  useEffect(() => {
    if (rule === null && machineId === "" && machines[0] !== undefined) {
      setMachineId(machines[0].machineId);
    }
  }, [machines, rule, machineId]);

  const selectedMachine = machines.find((machine) => machine.machineId === machineId);
  const selectedMachineHasNoMetrics =
    selectedMachine !== undefined &&
    selectedMachine.metrics === undefined;
  const presetResolutions = rule === null ? resolveAlertPresets(selectedMachine) : [];
  const selectedPreset =
    presetId === "" ? null : presetResolutions.find((item) => item.preset.id === presetId) ?? null;

  useEffect(() => {
    if (selectedMachineHasNoMetrics && rule === null && condition !== "stale") {
      setCondition("stale");
    }
  }, [selectedMachineHasNoMetrics, rule, condition]);

  const resources = resourceOptions(metric, selectedMachine);
  const needsThreshold = condition === "above" || condition === "below";
  const needsResource = alertMetricNeedsResource(metric);
  const disabled = busy || saving || machines.length === 0;

  const chooseMetric = (next: AlertMetric) => {
    setMetric(next);
    const nextResources = resourceOptions(next, selectedMachine);
    setResourceId(alertMetricNeedsResource(next) ? (nextResources[0]?.value ?? "") : "");
  };

  const chooseMachine = (next: string) => {
    setPresetId("");
    setMachineId(next);
    const machine = machines.find((item) => item.machineId === next);
    if (machine !== undefined && machine.metrics === undefined && rule === null) {
      setCondition("stale");
    }
    const nextResources = resourceOptions(metric, machine);
    if (alertMetricNeedsResource(metric)) {
      setResourceId(nextResources[0]?.value ?? "");
    }
  };

  const applyPreset = () => {
    if (selectedPreset === null || selectedPreset.input === null) return;
    const input = selectedPreset.input;
    setName(input.name);
    setMachineId(input.machineId);
    setMetric(input.metric);
    setResourceId(input.resourceId ?? "");
    setCondition(input.condition);
    setThreshold(
      input.threshold === null || input.threshold === undefined ? "" : String(input.threshold),
    );
    setDurationSeconds(String(input.durationMs / 1000));
    setError(null);
  };

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError(null);
    if (name.trim() === "") {
      setError("Give the rule a name.");
      return;
    }
    if (machineId === "") {
      setError("Choose a machine.");
      return;
    }
    if (selectedMachineHasNoMetrics && condition !== "stale") {
      setError(
        "No metrics have been received from this machine yet, so value and unavailable rules cannot be evaluated against it. Use a stale rule, or wait until the machine reports metrics.",
      );
      return;
    }
    if (needsResource && resourceId.trim() === "") {
      setError("Choose or enter the metric's resource.");
      return;
    }
    const seconds = Number(durationSeconds);
    if (durationSeconds.trim() === "" || !Number.isInteger(seconds) || seconds < 0 || seconds * 1000 > ALERT_LIMITS.maxDurationMs) {
      setError("Duration must be a whole number of seconds from 0 to 86400.");
      return;
    }
    let thresholdValue: number | null = null;
    if (needsThreshold) {
      if (threshold.trim() === "") {
        setError("Enter a numeric threshold.");
        return;
      }
      thresholdValue = Number(threshold);
      if (!Number.isFinite(thresholdValue)) {
        setError("Enter a numeric threshold.");
        return;
      }
    }
    const maintenanceUntil = fromDateTimeLocal(maintenanceLocal);
    setSaving(true);
    try {
      await onSave({
        ...(rule === null ? {} : {id: rule.id}),
        name: name.trim(),
        machineId,
        metric,
        resourceId: needsResource ? resourceId.trim() : "",
        condition,
        threshold: needsThreshold ? thresholdValue : null,
        durationMs: seconds * 1000,
        enabled,
        maintenanceUntil,
      });
    } catch (saveError) {
      setError(saveError instanceof Error ? saveError.message : "Could not save the rule.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <form onSubmit={(event) => void submit(event)} aria-busy={saving} className="flex flex-col gap-4">
      {machines.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          No machines available.
        </p>
      ) : null}

      {rule === null ? (
        <div className="border-b border-border pb-4">
          <div className="flex flex-col gap-2 sm:flex-row sm:items-end">
            <div className="min-w-0 flex-1">
              <label htmlFor="alert-rule-preset" className="text-xs text-muted-foreground">
                Warning preset
              </label>
              <select
                id="alert-rule-preset"
                value={presetId}
                onChange={(event) => setPresetId(event.target.value)}
                disabled={disabled}
                aria-describedby="alert-rule-preset-note"
                className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
              >
                <option value="">Choose a preset…</option>
                {presetResolutions.map((item) => (
                  <option key={item.preset.id} value={item.preset.id}>
                    {item.preset.label}
                    {item.status.kind === "ready"
                      ? ""
                      : item.status.kind === "waiting"
                        ? " — waiting for metrics"
                        : " — unavailable"}
                  </option>
                ))}
              </select>
            </div>
            <button
              type="button"
              onClick={applyPreset}
              disabled={disabled || selectedPreset?.status.kind !== "ready"}
              className="rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
            >
              Use preset
            </button>
          </div>
          <p
            id="alert-rule-preset-note"
            aria-live="polite"
            className="mt-1.5 text-xs text-muted-foreground"
          >
            {selectedPreset !== null && selectedPreset.status.kind !== "ready" ? selectedPreset.status.reason : ""}
          </p>
        </div>
      ) : null}

      <div className="grid gap-3 sm:grid-cols-2">
        <div className="sm:col-span-2">
          <label htmlFor="alert-rule-name" className="text-xs text-muted-foreground">
            Rule name
          </label>
          <input
            id="alert-rule-name"
            value={name}
            onChange={(event) => setName(event.target.value)}
            maxLength={ALERT_LIMITS.maxRuleName}
            disabled={disabled}
            placeholder="CPU sustained high"
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground placeholder:text-muted-foreground"
          />
        </div>

        <div>
          <label htmlFor="alert-rule-machine" className="text-xs text-muted-foreground">
            Machine
          </label>
          <select
            id="alert-rule-machine"
            value={machineId}
            onChange={(event) => chooseMachine(event.target.value)}
            disabled={disabled}
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
          >
            {machineId !== "" && !machines.some((machine) => machine.machineId === machineId) ? (
              <option value={machineId}>{machineId} (current)</option>
            ) : null}
            {machines.map((machine) => (
              <option key={machine.machineId} value={machine.machineId}>
                {machine.displayName?.trim() || machine.hostname || machine.machineId}
              </option>
            ))}
          </select>
        </div>

        <div>
          <label htmlFor="alert-rule-metric" className="text-xs text-muted-foreground">
            Metric
          </label>
          <select
            id="alert-rule-metric"
            value={metric}
            onChange={(event) => chooseMetric(event.target.value as AlertMetric)}
            disabled={disabled}
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
          >
            {metricOptions().map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </div>

        {needsResource ? (
          <div className="sm:col-span-2">
            <label htmlFor="alert-rule-resource" className="text-xs text-muted-foreground">
              Resource
            </label>
            {resources.length > 0 ? (
              <select
                id="alert-rule-resource"
                value={resourceId}
                onChange={(event) => setResourceId(event.target.value)}
                disabled={disabled}
                className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
              >
                {resourceId !== "" && !resources.some((option) => option.value === resourceId) ? (
                  <option value={resourceId}>{resourceId} (current)</option>
                ) : null}
                {resources.map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
                  </option>
                ))}
              </select>
            ) : (
              <input
                id="alert-rule-resource"
                value={resourceId}
                onChange={(event) => setResourceId(event.target.value)}
                maxLength={ALERT_LIMITS.maxResourceId}
                disabled={disabled}
                placeholder={placeholderFor(metric)}
                className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground placeholder:text-muted-foreground"
              />
            )}
          </div>
        ) : null}

        <div>
          <label htmlFor="alert-rule-condition" className="text-xs text-muted-foreground">
            Condition
          </label>
          <select
            id="alert-rule-condition"
            value={condition}
            onChange={(event) => setCondition(event.target.value as AlertCondition)}
            disabled={disabled}
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
          >
            {ALERT_CONDITIONS.map((option) => (
              <option
                key={option.value}
                value={option.value}
                disabled={selectedMachineHasNoMetrics && option.value !== "stale"}
              >
                {option.label}
              </option>
            ))}
          </select>
          {selectedMachineHasNoMetrics ? (
            <p className="mt-1 text-xs text-muted-foreground">
              Waiting for metrics.
            </p>
          ) : null}
        </div>

        {needsThreshold ? (
          <div>
            <label htmlFor="alert-rule-threshold" className="text-xs text-muted-foreground">
              Threshold ({unitHint(metric)})
            </label>
            <input
              id="alert-rule-threshold"
              type="number"
              step="any"
              value={threshold}
              onChange={(event) => setThreshold(event.target.value)}
              disabled={disabled}
              className="tnum mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
            />
          </div>
        ) : null}

        <div>
          <label htmlFor="alert-rule-duration" className="text-xs text-muted-foreground">
            Must hold for (seconds)
          </label>
          <input
            id="alert-rule-duration"
            type="number"
            min={0}
            max={ALERT_LIMITS.maxDurationMs / 1000}
            step={1}
            value={durationSeconds}
            onChange={(event) => setDurationSeconds(event.target.value)}
            disabled={disabled}
            className="tnum mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
          />
        </div>

        <div>
          <label htmlFor="alert-rule-maintenance" className="text-xs text-muted-foreground">
            Maintenance until (optional)
          </label>
          <input
            id="alert-rule-maintenance"
            type="datetime-local"
            value={maintenanceLocal}
            onChange={(event) => setMaintenanceLocal(event.target.value)}
            disabled={disabled}
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
          />
        </div>

        <label className="flex items-center gap-2 text-xs text-foreground sm:col-span-2">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(event) => setEnabled(event.target.checked)}
            disabled={disabled}
            className="h-4 w-4 rounded border-border accent-[var(--accent)]"
          />
          Rule enabled
        </label>
      </div>

      {error ? (
        <p role="alert" className="rounded-md bg-crit-soft px-3 py-2 text-xs text-foreground">
          {error}
        </p>
      ) : null}

      <div className="flex flex-wrap items-center gap-2">
        <button
          type="submit"
          disabled={disabled}
          className="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-accent-foreground transition-colors hover:bg-accent/90 disabled:opacity-50"
        >
          {saving ? "Saving…" : rule === null ? "Create rule" : "Save rule"}
        </button>
        {rule !== null ? (
          <button
            type="button"
            onClick={onCancel}
            disabled={saving}
            className="rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
          >
            Cancel
          </button>
        ) : null}
      </div>
    </form>
  );
}
