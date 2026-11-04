"use client";

/** Admin-only webhook configuration; secrets are write-only. */
import { useEffect, useRef, useState } from "react";

import {
  ALERT_LIMITS,
  type AlertWebhookConfigureInput,
  type AlertWebhookStatus,
  type AlertWebhookTestResult,
} from "@/alerts/types";

type Confirmation = "clear" | "replace" | null;

export function AlertWebhookPanel({
  status,
  busy,
  onConfigure,
  onClear,
  onTest,
}: {
  status: AlertWebhookStatus;
  busy: boolean;
  onConfigure: (input: AlertWebhookConfigureInput) => Promise<void>;
  onClear: () => Promise<void>;
  onTest: () => Promise<AlertWebhookTestResult>;
}) {
  const [url, setUrl] = useState("");
  const [headersJson, setHeadersJson] = useState("");
  const [secret, setSecret] = useState("");
  const [enabled, setEnabled] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<AlertWebhookTestResult | null>(null);
  const [working, setWorking] = useState(false);
  const [confirmation, setConfirmation] = useState<Confirmation>(null);
  const [pendingConfigure, setPendingConfigure] = useState<AlertWebhookConfigureInput | null>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  const replaceRef = useRef<HTMLButtonElement>(null);
  const clearRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (confirmation !== null) confirmRef.current?.focus();
  }, [confirmation]);

  useEffect(() => {
    if (!status.configured) {
      setConfirmation(null);
      setPendingConfigure(null);
    }
  }, [status.configured]);

  const buildInput = (): AlertWebhookConfigureInput | null => {
    setError(null);
    setTestResult(null);
    const trimmedUrl = url.trim();
    if (trimmedUrl === "") {
      setError("Enter the webhook URL.");
      return null;
    }
    let headers: Record<string, string> | undefined;
    if (headersJson.trim() !== "") {
      try {
        const parsed = JSON.parse(headersJson) as unknown;
        if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
          throw new Error("not an object");
        }
        headers = {};
        for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
          if (typeof value !== "string") throw new Error("header values must be strings");
          headers[key] = value;
        }
      } catch {
        setError("Headers must be a JSON object with string values.");
        return null;
      }
    }
    return {
      url: trimmedUrl,
      ...(headers === undefined ? {} : {headers}),
      secret: secret.trim() === "" ? null : secret,
      enabled,
    };
  };

  const commitConfigure = async (input: AlertWebhookConfigureInput) => {
    setWorking(true);
    setError(null);
    setConfirmation(null);
    setPendingConfigure(null);
    try {
      await onConfigure(input);
      setUrl("");
      setHeadersJson("");
      setSecret("");
      setEnabled(true);
    } catch (configureError) {
      setError(configureError instanceof Error ? configureError.message : "Could not configure the webhook.");
    } finally {
      setWorking(false);
    }
  };

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const input = buildInput();
    if (input === null) return;
    if (status.configured) {
      setPendingConfigure(input);
      setConfirmation("replace");
      return;
    }
    await commitConfigure(input);
  };

  const clear = async () => {
    setWorking(true);
    setError(null);
    setTestResult(null);
    setConfirmation(null);
    try {
      await onClear();
      setUrl("");
      setHeadersJson("");
      setSecret("");
    } catch (clearError) {
      setError(clearError instanceof Error ? clearError.message : "Could not clear the webhook.");
    } finally {
      setWorking(false);
    }
  };

  const confirm = async () => {
    if (confirmation === "clear") {
      await clear();
      return;
    }
    if (confirmation === "replace" && pendingConfigure !== null) {
      await commitConfigure(pendingConfigure);
    }
  };

  const cancelConfirmation = () => {
    const target = confirmation === "clear" ? clearRef.current : replaceRef.current;
    setConfirmation(null);
    setPendingConfigure(null);
    target?.focus();
  };

  const test = async () => {
    setError(null);
    setWorking(true);
    try {
      setTestResult(await onTest());
    } catch (testError) {
      setError(testError instanceof Error ? testError.message : "Could not test the webhook.");
    } finally {
      setWorking(false);
    }
  };

  const disabled = busy || working;
  const formDisabled = disabled || confirmation !== null;
  const loopbackTest = status.configured && status.transport === "loopback-http";

  return (
    <div className="flex flex-col gap-4">
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
        <dt className="text-muted-foreground">Configured</dt>
        <dd className="text-foreground">
          {status.configured ? (status.enabled ? "Enabled" : "Disabled") : "Not configured"}
        </dd>
        <dt className="text-muted-foreground">Transport</dt>
        <dd className="text-foreground">
          {status.transport === "https"
            ? "HTTPS"
            : status.transport === "loopback-http"
              ? "Loopback HTTP test sink"
              : "—"}
        </dd>
        <dt className="text-muted-foreground">Last test</dt>
        <dd className="text-foreground">
          {status.lastTestAt === null
            ? "Never"
            : `${status.lastTestOk === true ? "Passed" : "Failed"} · ${new Date(status.lastTestAt).toLocaleString()}`}
        </dd>
        {status.lastError ? (
          <>
            <dt className="text-muted-foreground">Last error</dt>
            <dd className="text-warn">{status.lastError}</dd>
          </>
        ) : null}
      </dl>

      <form onSubmit={(event) => void submit(event)} className="flex flex-col gap-3">
        <div>
          <label htmlFor="alert-webhook-url" className="text-xs text-muted-foreground">
            {status.configured ? "Replace webhook URL" : "Webhook URL"}
          </label>
          <input
            id="alert-webhook-url"
            type="url"
            required
            value={url}
            onChange={(event) => setUrl(event.target.value)}
            maxLength={ALERT_LIMITS.maxWebhookUrl}
            disabled={formDisabled}
            placeholder="https://receiver.example/alerts"
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground placeholder:text-muted-foreground"
          />
        </div>
        <div>
          <label htmlFor="alert-webhook-headers" className="text-xs text-muted-foreground">
            Headers (JSON, optional)
          </label>
          <textarea
            id="alert-webhook-headers"
            value={headersJson}
            onChange={(event) => setHeadersJson(event.target.value)}
            rows={2}
            disabled={formDisabled}
            placeholder={'{"x-channel": "monitoring"}'}
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 font-mono text-xs text-foreground placeholder:text-muted-foreground"
          />
        </div>
        <div>
          <label htmlFor="alert-webhook-secret" className="text-xs text-muted-foreground">
            Signing secret (optional)
          </label>
          <input
            id="alert-webhook-secret"
            type="password"
            value={secret}
            onChange={(event) => setSecret(event.target.value)}
            maxLength={ALERT_LIMITS.maxWebhookSecret}
            disabled={formDisabled}
            placeholder="Blank clears the signature"
            autoComplete="new-password"
            className="mt-1 w-full rounded-md border border-border bg-card px-2.5 py-1.5 text-sm text-foreground"
          />
        </div>
        <label className="flex items-center gap-2 text-xs text-foreground">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(event) => setEnabled(event.target.checked)}
            disabled={formDisabled}
            className="h-4 w-4 rounded border-border accent-[var(--accent)]"
          />
          Webhook enabled
        </label>

        {error ? (
          <p role="alert" className="rounded-md bg-crit-soft px-3 py-2 text-xs text-foreground">
            {error}
          </p>
        ) : null}
        <p
          role="status"
          className={testResult ? `rounded-md px-3 py-2 text-xs ${testResult.ok ? "bg-ok-soft text-foreground" : "bg-warn-soft text-foreground"}` : "sr-only"}
        >
          {testResult
            ? `Test ${testResult.ok ? "delivered" : "failed"}${testResult.status !== null ? ` (HTTP ${testResult.status})` : ""}${testResult.error ? ` — ${testResult.error}` : ""}.`
            : ""}
        </p>

        <div className="flex flex-wrap items-center gap-2">
          <button
            ref={replaceRef}
            type="submit"
            disabled={formDisabled || url.trim() === ""}
            className="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-accent-foreground transition-colors hover:bg-accent/90 disabled:opacity-50"
          >
            {status.configured ? "Replace webhook" : "Configure webhook"}
          </button>
          <button
            ref={clearRef}
            type="button"
            onClick={() => {
              setError(null);
              setConfirmation("clear");
            }}
            disabled={formDisabled || !status.configured}
            className="rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
          >
            Clear webhook
          </button>
          <button
            type="button"
            onClick={() => void test()}
            disabled={formDisabled || !loopbackTest}
            title={
              loopbackTest
                ? "Send one explicit test message to the configured loopback sink"
                : "Test delivery is available only for a configured loopback HTTP sink"
            }
            className="rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
          >
            Send test
          </button>
        </div>

        {confirmation !== null ? (
          <div
            role="group"
            aria-label="Confirm webhook change"
            className="rounded-md bg-warn-soft px-3 py-3"
          >
            <p className="text-xs text-foreground">
              {confirmation === "clear"
                ? "Clear the configured webhook? Future incidents will not be delivered until a webhook is configured again."
                : "Replace the configured webhook? The stored URL, headers and secret will be overwritten."}
            </p>
            <div className="mt-3 flex flex-wrap items-center gap-2">
              <button
                ref={confirmRef}
                type="button"
                onClick={() => void confirm()}
                disabled={working}
                className={`rounded-md px-3 py-1.5 text-xs font-medium text-accent-foreground transition-colors disabled:opacity-50 ${
                  confirmation === "clear" ? "bg-crit" : "bg-accent hover:bg-accent/90"
                }`}
              >
                {confirmation === "clear" ? "Confirm clear" : "Confirm replace"}
              </button>
              <button
                type="button"
                onClick={cancelConfirmation}
                disabled={working}
                className="rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50"
              >
                Cancel
              </button>
            </div>
          </div>
        ) : null}
      </form>
    </div>
  );
}
