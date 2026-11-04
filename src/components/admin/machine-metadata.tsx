"use client";

import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { RefreshCw, Save, Server } from "lucide-react";

import type { AdminApi } from "@/admin/api";
import {
  formatMachineLabel,
  parseMachineMetaDraft,
  type AdminMachine,
} from "@/admin/presentation";
import { EmptyState, Panel } from "@/components/panel";
import {
  ADMIN_BUTTON_PRIMARY,
  ADMIN_INPUT,
  Field,
  InlineNotice,
  describedBy,
  friendlyError,
  isForbidden,
  isUnauthorized,
} from "./admin-ui";

export function MachineMetadata(props: {
  api: AdminApi;
  csrfToken: string;
  machines: AdminMachine[];
  machinesPending: boolean;
  onSessionLost: () => void;
}) {
  const { api, csrfToken, machines, machinesPending, onSessionLost } = props;
  const prefix = useId();
  const fid = (name: string) => `${prefix}-${name}`;
  const [selectedId, setSelectedId] = useState("");
  const [name, setName] = useState("");
  const [tagsRaw, setTagsRaw] = useState("");
  const [dirty, setDirty] = useState(false);
  const [errors, setErrors] = useState<{ displayName?: string; tags?: string }>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [announcement, setAnnouncement] = useState("");
  const errorRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (machines.length === 0) {
      if (selectedId !== "") setSelectedId("");
      return;
    }
    if (machines.some((machine) => machine.machineId === selectedId)) return;
    const first = machines[0]!;
    setSelectedId(first.machineId);
    setName(first.displayName ?? "");
    setTagsRaw((first.tags ?? []).join(", "));
    setDirty(false);
  }, [machines, selectedId]);

  useEffect(() => {
    if (formError) errorRef.current?.focus();
  }, [formError]);

  const selected = machines.find((machine) => machine.machineId === selectedId);

  const selectMachine = (machineId: string) => {
    const machine = machines.find((item) => item.machineId === machineId);
    setSelectedId(machineId);
    setName(machine?.displayName ?? "");
    setTagsRaw((machine?.tags ?? []).join(", "));
    setDirty(false);
    setErrors({});
    setFormError(null);
    setAnnouncement("");
  };

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!selected || saving) return;
    setFormError(null);
    const parsed = parseMachineMetaDraft(name, tagsRaw);
    setErrors(parsed.errors);
    if (parsed.errors.displayName) {
      document.getElementById(fid("name"))?.focus();
      return;
    }
    if (parsed.errors.tags) {
      document.getElementById(fid("tags"))?.focus();
      return;
    }
    setSaving(true);
    try {
      await api.saveMachineMeta(
        selected.machineId,
        { displayName: parsed.displayName, tags: parsed.tags },
        csrfToken,
      );
      setName(parsed.displayName ?? "");
      setTagsRaw(parsed.tags.join(", "));
      setDirty(false);
      setAnnouncement(
        `Saved metadata for ${formatMachineLabel(selected)}. Fleet and machine views update from the live stream.`,
      );
    } catch (error) {
      if (isUnauthorized(error)) {
        onSessionLost();
        return;
      }
      setFormError(
        isForbidden(error)
          ? "You do not have access to this machine, so its metadata cannot be changed."
          : friendlyError(error, "Could not save machine metadata."),
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <Panel title="Machine metadata" className="min-w-0">
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

      {machines.length === 0 ? (
        <EmptyState
          icon={Server}
          note={machinesPending ? "Waiting for the live machine list…" : "No machines are visible to your account."}
        />
      ) : (
        <form
          aria-busy={saving}
          className="flex min-w-0 flex-col gap-4"
          onSubmit={(event) => void submit(event)}
        >
          <Field id={fid("machine")} label="Machine">
            <select
              id={fid("machine")}
              value={selectedId}
              onChange={(event) => selectMachine(event.target.value)}
              className={ADMIN_INPUT}
            >
              {machines.map((machine) => (
                <option key={machine.machineId} value={machine.machineId}>
                  {formatMachineLabel(machine)}
                </option>
              ))}
            </select>
          </Field>

          <Field
            id={fid("name")}
            label="Display name"
            error={errors.displayName}
          >
            <input
              id={fid("name")}
              placeholder={selected?.hostname || "Hostname"}
              maxLength={128}
              value={name}
              onChange={(event) => {
                setName(event.target.value);
                setDirty(true);
              }}
              aria-invalid={errors.displayName ? true : undefined}
              aria-describedby={describedBy(
                fid("name"),
                errors.displayName,
              )}
              className={ADMIN_INPUT}
            />
          </Field>

          <Field
            id={fid("tags")}
            label="Tags (comma-separated)"
            error={errors.tags}
          >
            <input
              id={fid("tags")}
              value={tagsRaw}
              onChange={(event) => {
                setTagsRaw(event.target.value);
                setDirty(true);
              }}
              aria-invalid={errors.tags ? true : undefined}
              aria-describedby={describedBy(
                fid("tags"),
                errors.tags,
              )}
              className={ADMIN_INPUT}
            />
          </Field>

          {formError ? (
            <div ref={errorRef} tabIndex={-1} aria-live="assertive">
              <InlineNotice tone="error" role="alert">
                {formError}
              </InlineNotice>
            </div>
          ) : null}

          <div className="flex flex-wrap items-center gap-2">
            <button type="submit" className={ADMIN_BUTTON_PRIMARY} disabled={saving || !dirty}>
              {saving ? (
                <>
                  <RefreshCw className="h-3.5 w-3.5 animate-spin" aria-hidden />
                  Saving…
                </>
              ) : (
                <>
                  <Save className="h-3.5 w-3.5" aria-hidden />
                  Save metadata
                </>
              )}
            </button>
          </div>
        </form>
      )}
    </Panel>
  );
}
