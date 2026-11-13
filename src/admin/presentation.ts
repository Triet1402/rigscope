/**
 * Browser-safe presentation and validation helpers for Administration.
 *
 * These are intentionally free of React, DOM and Node APIs so the same rules
 * drive the UI, the tests, and (by inspection) the server surface. Validation
 * happens on submit; the hub remains authoritative for conflicts such as the
 * last active administrator.
 */

import type { AdminScope } from "./api";

export const MAX_USERS = 50;
export const MAX_TAGS = 32;
export const MAX_TAG_LENGTH = 64;
export const MAX_DISPLAY_NAME = 128;
export const MAX_USERNAME_LENGTH = 128;
export const MAX_PASSWORD_LENGTH = 1024;
/**
 * Explicit machine scopes are bounded by bytes, not by a machine count. This
 * mirrors the administrative request budget: well over fifty identifiers fit,
 * while an oversized draft fails before the hub rejects it.
 */
export const MAX_SCOPE_BYTES = 8 * 1024;

const IDENTIFIER = /^[\x21-\x7e]{1,128}$/;
const CONTROL = /[\u0000-\u001f\u007f]/;

export type AdminMachine = {
  machineId: string;
  hostname?: string | null;
  displayName?: string | null;
  tags?: string[];
};

export type UserDraft = {
  username: string;
  password: string;
  machineIds: AdminScope;
  admin: boolean;
  disabled: boolean;
};

export type UserDraftErrors = {
  username?: string;
  password?: string;
  scope?: string;
};

export type UserDraftValidation = {
  errors: UserDraftErrors;
  firstInvalid: "username" | "password" | "scope" | null;
};

/** Tags are a comma-separated operator input; order is preserved, duplicates removed. */
export function parseTagsInput(raw: string): { tags: string[]; error: string | null } {
  const pieces = raw
    .split(",")
    .map((piece) => piece.trim())
    .filter((piece) => piece.length > 0);
  const tags: string[] = [];
  const seen = new Set<string>();
  for (const tag of pieces) {
    if (tag.length > MAX_TAG_LENGTH) {
      return { tags, error: `Each tag must be at most ${MAX_TAG_LENGTH} characters.` };
    }
    if (CONTROL.test(tag)) {
      return { tags, error: "Tags cannot contain control characters." };
    }
    if (seen.has(tag)) continue;
    seen.add(tag);
    tags.push(tag);
  }
  if (tags.length > MAX_TAGS) {
    return { tags, error: `Use at most ${MAX_TAGS} tags.` };
  }
  return { tags, error: null };
}

export function parseMachineMetaDraft(
  nameRaw: string,
  tagsRaw: string,
): {
  displayName: string | null;
  tags: string[];
  errors: { displayName?: string; tags?: string };
} {
  const trimmed = nameRaw.trim();
  const errors: { displayName?: string; tags?: string } = {};
  if (trimmed.length > MAX_DISPLAY_NAME) {
    errors.displayName = `Display name must be at most ${MAX_DISPLAY_NAME} characters.`;
  } else if (CONTROL.test(trimmed)) {
    errors.displayName = "Display name cannot contain control characters.";
  }
  const parsedTags = parseTagsInput(tagsRaw);
  if (parsedTags.error !== null) errors.tags = parsedTags.error;
  return {
    displayName: trimmed.length === 0 ? null : trimmed,
    tags: parsedTags.tags,
    errors,
  };
}

export function validateUserDraft(
  draft: UserDraft,
  options: { existing: boolean },
): UserDraftValidation {
  const errors: UserDraftErrors = {};

  if (!IDENTIFIER.test(draft.username)) {
    errors.username = "Use 1–128 printable characters without spaces.";
  }

  if (!options.existing && draft.password.length === 0) {
    errors.password = "Choose a password for the new user.";
  } else if (draft.password.length > MAX_PASSWORD_LENGTH) {
    errors.password = `Password must be at most ${MAX_PASSWORD_LENGTH} characters.`;
  }

  if (draft.machineIds === "*") {
    // All machines is always a valid scope.
  } else if (!Array.isArray(draft.machineIds)) {
    errors.scope = "Choose all machines or an explicit list of machine IDs.";
  } else {
    const seen = new Set<string>();
    let bytes = 2;
    for (const id of draft.machineIds) {
      if (!IDENTIFIER.test(id)) {
        errors.scope = "Every machine ID must be a valid identifier.";
        break;
      }
      bytes += id.length + 3;
      if (bytes > MAX_SCOPE_BYTES) {
        errors.scope = `Keep the explicit machine list under ${MAX_SCOPE_BYTES} bytes.`;
        break;
      }
      if (seen.has(id)) {
        errors.scope = "Machine scope contains a duplicate ID.";
        break;
      }
      seen.add(id);
    }
  }

  const firstInvalid = errors.username
    ? "username"
    : errors.password
      ? "password"
      : errors.scope
        ? "scope"
        : null;
  return { errors, firstInvalid };
}

export function scopeSummary(scope: AdminScope): string {
  if (scope === "*") return "All machines";
  if (scope.length === 0) return "No machines";
  if (scope.length === 1) return scope[0]!;
  return `${scope.length} machines`;
}

export function scopeIds(scope: AdminScope): string[] {
  return scope === "*" ? [] : [...scope];
}

export function unknownScopeIds(scope: AdminScope, knownMachineIds: readonly string[]): string[] {
  if (scope === "*") return [];
  const known = new Set(knownMachineIds);
  return scope.filter((id) => !known.has(id));
}

export function toggleScopeSelection(selected: readonly string[], machineId: string): string[] {
  return selected.includes(machineId)
    ? selected.filter((id) => id !== machineId)
    : [...selected, machineId];
}

export function formatAdminTimestamp(iso: string): string {
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return "Unknown time";
  return date.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

export function formatAuditAction(action: string): string {
  switch (action) {
    case "user.create":
      return "Created user";
    case "user.update":
      return "Updated access";
    case "user.update-password":
      return "Changed password";
    case "user.delete":
      return "Deleted user";
    default:
      return action.replace(/[-_.]+/g, " ");
  }
}

export function formatMachineLabel(machine: AdminMachine): string {
  const name = machine.displayName?.trim();
  if (name) return name;
  const hostname = machine.hostname?.trim();
  if (hostname) return hostname;
  return machine.machineId;
}

export type GrantMode = "new" | "replacement" | "reenroll";

export type EnrollmentTarget = {
  machineId: string;
  collectorId: string;
  /** A revoked mapping is re-enrolled; an active one is replaced. */
  mode: Exclude<GrantMode, "new">;
};

/**
 * Every registered mapping is a valid replacement-grant target. A revoked
 * collector keeps its identity locally and the hub refuses to enroll a known
 * collector id through a new-installation grant, so re-enrolling the revoked
 * mapping is the only way that machine can come back. Active mappings are
 * listed first.
 */
export function enrollmentTargets(
  collectors: readonly { machineId: string; collectorId: string; revoked: boolean }[],
): EnrollmentTarget[] {
  return [...collectors]
    .sort((a, b) => Number(a.revoked) - Number(b.revoked))
    .map((row) => ({
      machineId: row.machineId,
      collectorId: row.collectorId,
      mode: row.revoked ? "reenroll" : "replacement",
    }));
}

/** Quote one shell word so a pasted command cannot be split or expanded. */
function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

/**
 * The enrollment command is complete: the hub address and the one-time grant
 * are both in it, so the operator copies one thing. The grant is single-use
 * and short-lived, so its copy in shell history is spent once it is used. A new
 * machine runs the installer, which records the destination and stages the
 * grant. An existing installation enrolls explicitly; `collector enroll`
 * restarts a running collector itself, so no separate restart is needed.
 */
export function grantCommand(
  mode: GrantMode,
  origin = "https://your-hub",
  token = "<grant>",
): { command: string; note: string } {
  const url = new URL(origin);
  if (!["https:", "http:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw new Error("Invalid hub origin");
  }
  const address = shellQuote(url.origin);
  const grant = shellQuote(token);
  const notes: Record<GrantMode, string> = {
    new:
      "Run this on the new machine. It installs Rigscope as a collector, enrolls it with this hub and starts it. " +
      "The machine running this hub already has its own collector (one per installation); run this there only to bring back a collector that was removed.",
    replacement:
      "Run this on the existing machine. Its old credential keeps working until the new enrollment succeeds; a running collector restarts to use the new one.",
    reenroll:
      "Run this on the revoked machine. It keeps its machine history; a running collector restarts to use the new credential.",
  };
  const command =
    mode === "new"
      ? `curl -fsSL https://rigscope.dev/install.sh | sh -s -- --role collector --hub ${address} --token ${grant}`
      : `rigscope collector enroll --hub-url ${address} --grant ${grant}`;
  return { command, note: notes[mode] };
}
