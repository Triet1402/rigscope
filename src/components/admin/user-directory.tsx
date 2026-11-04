"use client";

import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type FormEvent,
} from "react";
import {
  AlertTriangle,
  Pencil,
  Plus,
  RefreshCw,
  ShieldCheck,
  Trash2,
  UserRound,
  X,
} from "lucide-react";

import type { AdminApi, AdminScope, AdminUserRecord } from "@/admin/api";
import {
  MAX_USERS,
  formatMachineLabel,
  scopeSummary,
  toggleScopeSelection,
  unknownScopeIds,
  validateUserDraft,
  type AdminMachine,
} from "@/admin/presentation";
import { EmptyState, Panel } from "@/components/panel";
import {
  ADMIN_BUTTON_DANGER,
  ADMIN_BUTTON_PRIMARY,
  ADMIN_BUTTON_QUIET,
  ADMIN_BUTTON_SECONDARY,
  ADMIN_INPUT,
  Badge,
  Field,
  InlineNotice,
  describedBy,
  friendlyError,
  isAbort,
  isForbidden,
  isUnauthorized,
} from "./admin-ui";

type EditorState = { mode: "create" } | { mode: "edit"; user: AdminUserRecord } | null;

export function UserDirectory(props: {
  api: AdminApi;
  username: string;
  csrfToken: string;
  machines: AdminMachine[];
  machinesPending: boolean;
  onAuditChange: () => void;
  onSessionLost: () => void;
}) {
  const { api, username, csrfToken, machines, machinesPending, onAuditChange, onSessionLost } = props;
  const [users, setUsers] = useState<AdminUserRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const [editor, setEditor] = useState<EditorState>(null);
  const [confirmDelete, setConfirmDelete] = useState<AdminUserRecord | null>(null);
  const [deletePending, setDeletePending] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      abortRef.current?.abort();
    };
  }, []);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoading(true);
    setLoadError(null);
    try {
      const next = await api.listUsers(controller.signal);
      if (!controller.signal.aborted) setUsers(next);
    } catch (error) {
      if (controller.signal.aborted || isAbort(error)) return;
      if (isUnauthorized(error)) {
        onSessionLost();
        return;
      }
      setLoadError(
        isForbidden(error)
          ? "Administrator access is required. If your access just changed, sign in again."
          : friendlyError(error, "Could not load the browser user directory."),
      );
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [api, onSessionLost]);

  useEffect(() => {
    void load();
  }, [load]);

  const handleSaved = (message: string) => {
    setEditor(null);
    setAnnouncement(message);
    onAuditChange();
    void load();
  };

  const handleDelete = async () => {
    if (!confirmDelete || deletePending) return;
    setDeletePending(true);
    setDeleteError(null);
    try {
      await api.deleteUser(confirmDelete.username, csrfToken);
      if (!mountedRef.current) return;
      const deleted = confirmDelete.username;
      setConfirmDelete(null);
      setAnnouncement(`Deleted user ${deleted}. Any active sessions were ended.`);
      onAuditChange();
      await load();
    } catch (error) {
      if (!mountedRef.current) return;
      if (isUnauthorized(error)) {
        onSessionLost();
        return;
      }
      setDeleteError(
        isForbidden(error)
          ? "Your account no longer has administrative access."
          : friendlyError(error, "Could not delete that user."),
      );
    } finally {
      if (mountedRef.current) setDeletePending(false);
    }
  };

  return (
    <Panel
      title="Browser users"
      className="min-w-0"
      action={
        <button
          type="button"
          className={ADMIN_BUTTON_PRIMARY}
          aria-expanded={editor?.mode === "create"}
          onClick={() => {
            setConfirmDelete(null);
            setDeleteError(null);
            setEditor((current) => (current?.mode === "create" ? null : { mode: "create" }));
          }}
        >
          <Plus className="h-3.5 w-3.5" aria-hidden />
          Add user
        </button>
      }
    >
      <p
        role="status"
        className={
          announcement
            ? "rounded-md bg-ok-soft px-3 py-2 text-xs text-foreground"
            : "sr-only"
        }
      >
        {announcement}
      </p>

      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="tnum text-xs text-muted-foreground">
            {users.length}/{MAX_USERS}
          </span>
          <button
            type="button"
            className={ADMIN_BUTTON_QUIET}
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} aria-hidden />
            Refresh
          </button>
        </div>
      </div>

      {editor ? (
        <UserEditor
          key={editor.mode === "edit" ? `edit-${editor.user.username}` : "create"}
          api={api}
          csrfToken={csrfToken}
          existing={editor.mode === "edit" ? editor.user : undefined}
          currentUsername={username}
          machines={machines}
          machinesPending={machinesPending}
          onCancel={() => setEditor(null)}
          onSaved={handleSaved}
          onSessionLost={onSessionLost}
        />
      ) : null}

      <div className="mt-4 flex min-w-0 flex-col gap-3">
        {loadError ? (
          <div className="flex flex-wrap items-center justify-between gap-2">
            <InlineNotice tone="error" role="alert">
              {loadError}
            </InlineNotice>
            <button type="button" className={ADMIN_BUTTON_SECONDARY} onClick={() => void load()}>
              Try again
            </button>
          </div>
        ) : null}

        {loading && users.length === 0 ? (
          <p role="status" className="py-6 text-sm text-muted-foreground">
            Loading browser users…
          </p>
        ) : null}

        {!loading && !loadError && users.length === 0 ? (
          <EmptyState
            icon={UserRound}
            note="No browser users are stored yet."
          />
        ) : null}

        {users.length > 0 ? (
          <ul className="divide-y divide-border border-t border-border">
            {users.map((user) => {
              const isSelf = user.username === username;
              const confirming = confirmDelete?.username === user.username;
              return (
                <li key={user.username} className="min-w-0 py-3">
                  <div className="flex min-w-0 flex-wrap items-start justify-between gap-x-4 gap-y-2">
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="break-words text-sm font-medium">{user.username}</span>
                        {isSelf ? <Badge tone="accent">You</Badge> : null}
                        {user.admin ? (
                          <Badge tone="ok">
                            <ShieldCheck className="h-3 w-3" aria-hidden />
                            Administrator
                          </Badge>
                        ) : (
                          <Badge>Viewer</Badge>
                        )}
                        {user.disabled ? <Badge tone="warn">Disabled</Badge> : null}
                      </div>
                      <p
                        className="mt-1 break-words text-xs text-muted-foreground"
                        title={user.machineIds === "*" ? "All machines" : user.machineIds.join(", ")}
                      >
                        {scopeSummary(user.machineIds)}
                      </p>
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      <button
                        type="button"
                        className={ADMIN_BUTTON_QUIET}
                        aria-expanded={
                          editor?.mode === "edit" && editor.user.username === user.username
                        }
                        onClick={() => {
                          setDeleteError(null);
                          setConfirmDelete(null);
                          setEditor(
                            editor?.mode === "edit" && editor.user.username === user.username
                              ? null
                              : { mode: "edit", user },
                          );
                        }}
                      >
                        <Pencil className="h-3.5 w-3.5" aria-hidden />
                        {editor?.mode === "edit" && editor.user.username === user.username
                          ? "Close editor"
                          : "Edit"}
                      </button>
                      <button
                        type="button"
                        className={ADMIN_BUTTON_QUIET}
                        aria-expanded={confirming}
                        onClick={() => {
                          setEditor(null);
                          setDeleteError(null);
                          setConfirmDelete(confirming ? null : user);
                        }}
                      >
                        <Trash2 className="h-3.5 w-3.5" aria-hidden />
                        Delete
                      </button>
                    </div>
                  </div>

                  {confirming ? (
                    <div
                      role="alert"
                      className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-md bg-crit-soft px-3 py-2"
                    >
                      <p className="min-w-0 flex-1 text-xs text-foreground">
                        Delete <strong className="font-medium">{user.username}</strong>? Their
                        active sessions end immediately, and this cannot be undone.
                      </p>
                      <div className="flex flex-wrap gap-1.5">
                        <button
                          type="button"
                          className={ADMIN_BUTTON_DANGER}
                          onClick={() => void handleDelete()}
                          disabled={deletePending}
                        >
                          {deletePending ? "Deleting…" : "Delete user"}
                        </button>
                        <button
                          type="button"
                          className={ADMIN_BUTTON_SECONDARY}
                          onClick={() => setConfirmDelete(null)}
                          disabled={deletePending}
                        >
                          Cancel
                        </button>
                      </div>
                    </div>
                  ) : null}
                  {confirming && deleteError ? (
                    <p role="alert" className="mt-2 text-xs text-crit">
                      {deleteError}
                    </p>
                  ) : null}
                </li>
              );
            })}
          </ul>
        ) : null}
      </div>
    </Panel>
  );
}

function UserEditor(props: {
  api: AdminApi;
  csrfToken: string;
  existing?: AdminUserRecord;
  currentUsername: string;
  machines: AdminMachine[];
  machinesPending: boolean;
  onCancel: () => void;
  onSaved: (message: string) => void;
  onSessionLost: () => void;
}) {
  const {
    api,
    csrfToken,
    existing,
    currentUsername,
    machines,
    machinesPending,
    onCancel,
    onSaved,
    onSessionLost,
  } = props;
  const prefix = useId();
  const fid = (name: string) => `${prefix}-${name}`;
  const headingId = fid("heading");

  const [username, setUsername] = useState(existing?.username ?? "");
  const [password, setPassword] = useState("");
  const [allMachines, setAllMachines] = useState(
    existing ? existing.machineIds === "*" : false,
  );
  const [selectedIds, setSelectedIds] = useState<string[]>(
    existing && existing.machineIds !== "*" ? [...existing.machineIds] : [],
  );
  const [admin, setAdmin] = useState(existing?.admin ?? false);
  const [disabled, setDisabled] = useState(existing?.disabled ?? false);
  const [errors, setErrors] = useState<{
    username?: string;
    password?: string;
    scope?: string;
  }>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const formErrorRef = useRef<HTMLDivElement | null>(null);
  const focusedRef = useRef(false);

  useEffect(() => {
    if (focusedRef.current) return;
    focusedRef.current = true;
    document.getElementById(headingId)?.focus();
  }, [headingId]);

  useEffect(() => {
    if (formError) formErrorRef.current?.focus();
  }, [formError]);

  const sortedMachines = [...machines].sort((a, b) => {
    const left = formatMachineLabel(a).toLocaleLowerCase();
    const right = formatMachineLabel(b).toLocaleLowerCase();
    return left.localeCompare(right) || a.machineId.localeCompare(b.machineId);
  });
  const knownIds = sortedMachines.map((machine) => machine.machineId);
  const unknownIds = unknownScopeIds(selectedIds, knownIds);
  const scope: AdminScope = allMachines ? "*" : selectedIds;
  const isSelf = existing?.username === currentUsername;

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (submitting) return;
    setFormError(null);

    const draft = {
      username: existing?.username ?? username,
      password,
      machineIds: scope,
      admin,
      disabled,
    };
    const validation = validateUserDraft(draft, { existing: existing !== undefined });
    setErrors(validation.errors);
    if (validation.firstInvalid !== null) {
      const target =
        validation.firstInvalid === "username"
          ? fid("username")
          : validation.firstInvalid === "password"
            ? fid("password")
            : fid("scope-all");
      document.getElementById(target)?.focus();
      return;
    }

    setSubmitting(true);
    try {
      await api.saveUser(
        {
          username: draft.username,
          machineIds: draft.machineIds,
          admin,
          disabled,
          ...(password.length > 0 ? { password } : {}),
        },
        csrfToken,
      );
      setPassword("");
      onSaved(
        existing
          ? `Updated ${existing.username}.`
          : `Created ${draft.username}. They can sign in with the password you set.`,
      );
    } catch (error) {
      if (isUnauthorized(error)) {
        onSessionLost();
        return;
      }
      setFormError(
        isForbidden(error)
          ? "Your account no longer has administrative access."
          : friendlyError(error, "Could not save that user."),
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <form
      aria-labelledby={headingId}
      aria-busy={submitting}
      className="flex min-w-0 flex-col gap-4 border-t border-border py-4"
      onSubmit={(event) => void submit(event)}
    >
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h3 id={headingId} tabIndex={-1} className="text-sm font-medium tracking-tight">
            {existing ? `Edit ${existing.username}` : "Add a browser user"}
          </h3>
        </div>
        <button type="button" className={ADMIN_BUTTON_QUIET} onClick={onCancel} disabled={submitting}>
          Close
        </button>
      </div>

      {isSelf ? (
        <InlineNotice tone="warn" role="status">
          <span className="flex items-start gap-2">
            <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            <span>
              Account changes may sign you out.
            </span>
          </span>
        </InlineNotice>
      ) : null}

      {formError ? (
        <div ref={formErrorRef} tabIndex={-1} aria-live="assertive">
          <InlineNotice tone="error" role="alert">
            {formError}
          </InlineNotice>
        </div>
      ) : null}

      <div className="grid min-w-0 gap-4 sm:grid-cols-2">
        {existing ? (
          <div className="flex min-w-0 flex-col gap-1.5">
            <span className="text-xs font-medium text-foreground">Username</span>
            <p className="tnum break-words rounded-md border border-border bg-card-strong px-3 py-2 text-sm">
              {existing.username}
            </p>
          </div>
        ) : (
          <Field
            id={fid("username")}
            label="Username"
            error={errors.username}
          >
            <input
              id={fid("username")}
              name="username"
              autoComplete="username"
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              aria-invalid={errors.username ? true : undefined}
              aria-describedby={describedBy(fid("username"), errors.username)}
              className={ADMIN_INPUT}
            />
          </Field>
        )}

        <Field
          id={fid("password")}
          label={existing ? "New password (optional)" : "Password"}
          error={errors.password}
        >
          <input
            id={fid("password")}
            name="new-password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            aria-invalid={errors.password ? true : undefined}
            aria-describedby={describedBy(
              fid("password"),
              errors.password,
            )}
            className={ADMIN_INPUT}
          />
        </Field>
      </div>

      <fieldset
        className="flex min-w-0 flex-col gap-2"
        aria-invalid={errors.scope ? true : undefined}
        aria-describedby={errors.scope ? fid("scope-error") : undefined}
      >
        <legend className="text-xs font-medium text-foreground">Machine access</legend>
        <div className="flex flex-wrap gap-x-5 gap-y-2">
          <label className="flex items-center gap-2 text-xs text-foreground">
            <input
              id={fid("scope-all")}
              type="radio"
              name={fid("scope")}
              checked={allMachines}
              onChange={() => setAllMachines(true)}
            />
            All machines
          </label>
          <label className="flex items-center gap-2 text-xs text-foreground">
            <input
              id={fid("scope-selected")}
              type="radio"
              name={fid("scope")}
              checked={!allMachines}
              onChange={() => setAllMachines(false)}
            />
            Selected machines
          </label>
        </div>

        {!allMachines ? (
          <div className="flex min-w-0 flex-col gap-2 border-t border-border pt-2">
            {sortedMachines.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                {machinesPending
                  ? "Waiting for machines…"
                  : "No machines available."}
              </p>
            ) : (
              <div className="max-h-56 overflow-y-auto pr-1">
                <ul className="flex flex-col gap-1.5">
                  {sortedMachines.map((machine) => (
                    <li key={machine.machineId} className="flex items-start gap-2">
                      <input
                        id={fid(`machine-${machine.machineId}`)}
                        type="checkbox"
                        className="mt-0.5"
                        checked={selectedIds.includes(machine.machineId)}
                        onChange={() =>
                          setSelectedIds((current) =>
                            toggleScopeSelection(current, machine.machineId),
                          )
                        }
                      />
                      <label
                        htmlFor={fid(`machine-${machine.machineId}`)}
                        className="min-w-0 text-xs text-foreground"
                      >
                        <span className="block break-words">{formatMachineLabel(machine)}</span>
                        <span className="tnum block break-all text-muted-foreground">
                          {machine.machineId}
                        </span>
                      </label>
                    </li>
                  ))}
                </ul>
              </div>
            )}

            {unknownIds.length > 0 ? (
              <div className="flex flex-col gap-1.5">
                <p className="text-xs text-muted-foreground">
                  Other saved machines
                </p>
                <ul className="flex flex-wrap gap-1.5">
                  {unknownIds.map((id) => (
                    <li
                      key={id}
                      className="flex items-center gap-1.5 rounded-full border border-border px-2 py-0.5 text-xs"
                    >
                      <span className="tnum break-all">{id}</span>
                      <button
                        type="button"
                        className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-muted-foreground hover:bg-card-strong hover:text-foreground"
                        onClick={() =>
                          setSelectedIds((current) => current.filter((item) => item !== id))
                        }
                        aria-label={`Remove ${id} from the machine scope`}
                      >
                        <X className="h-3 w-3" aria-hidden />
                      </button>
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>
        ) : null}

        {errors.scope ? (
          <p id={fid("scope-error")} className="text-xs text-crit">
            {errors.scope}
          </p>
        ) : null}
      </fieldset>

      <div className="flex flex-col gap-2 border-t border-border pt-3">
        <label className="flex items-start gap-2 text-xs text-foreground">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={admin}
            onChange={(event) => setAdmin(event.target.checked)}
          />
          <span>
            Administrator
            <span className="block text-muted-foreground">
              Explicit administrative access. A wildcard machine scope alone never grants it.
            </span>
          </span>
        </label>
        <label className="flex items-start gap-2 text-xs text-foreground">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={disabled}
            onChange={(event) => setDisabled(event.target.checked)}
          />
          <span>
            Disabled
            <span className="block text-muted-foreground">
              Blocks sign-in and ends active sessions. The account is kept for a later re-enable.
            </span>
          </span>
        </label>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <button type="submit" className={ADMIN_BUTTON_PRIMARY} disabled={submitting}>
          {submitting ? "Saving…" : existing ? "Save changes" : "Create user"}
        </button>
        <button type="button" className={ADMIN_BUTTON_SECONDARY} onClick={onCancel} disabled={submitting}>
          Cancel
        </button>
      </div>
    </form>
  );
}
