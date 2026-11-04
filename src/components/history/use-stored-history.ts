"use client";

import { useEffect, useRef, useState } from "react";
import type { HistoryResult } from "@/hub/storage/history-query";
import { historyRequestUrl, parseHistoryResult, refreshIntervalMs, type RangeId } from "@/history/presentation";

type HistoryErrorKind =
  | "unauthorized"
  | "denied"
  | "missing"
  | "rate-limited"
  | "unavailable"
  | "network"
  | "invalid";

export type HistoryError = { kind: HistoryErrorKind; message: string };

type LoadedHistory = { key: string; range: RangeId; result: HistoryResult; loadedAt: number };

const HISTORY_REQUEST_TIMEOUT_MS = 20_000;

function requestSignal(controller: AbortController): AbortSignal {
  if (typeof AbortSignal.any !== "function" || typeof AbortSignal.timeout !== "function") {
    return controller.signal;
  }
  return AbortSignal.any([controller.signal, AbortSignal.timeout(HISTORY_REQUEST_TIMEOUT_MS)]);
}

function isHistoryError(value: unknown): value is HistoryError {
  return (
    value !== null &&
    typeof value === "object" &&
    "kind" in value &&
    "message" in value &&
    typeof (value as { message: unknown }).message === "string"
  );
}

async function responseError(res: Response): Promise<HistoryError> {
  let detail: string | null = null;
  try {
    const body: unknown = await res.json();
    if (body !== null && typeof body === "object" && "error" in body) {
      const value = (body as { error: unknown }).error;
      if (typeof value === "string") detail = value;
    }
  } catch {
    // Non-JSON error body.
  }
  switch (res.status) {
    case 401:
      return { kind: "unauthorized", message: "Your session is no longer accepted." };
    case 403:
      return {
        kind: "denied",
        message: detail ?? "You do not have access to this machine's history.",
      };
    case 404:
      return { kind: "missing", message: detail ?? "This machine is not available to the hub." };
    case 429:
      return { kind: "rate-limited", message: detail ?? "The hub is limiting history requests." };
    case 503:
      return { kind: "unavailable", message: detail ?? "History is temporarily unavailable." };
    default:
      return {
        kind: "invalid",
        message: detail ?? `The hub could not answer this history query (${res.status}).`,
      };
  }
}

/** Visible-tab polling keeps the last result while refreshing and never overlaps requests. */
export function useStoredHistory({ machineId, metricsKey, coreIdsKey, range, enabled = true, autoRefresh = true, refreshNonce = 0 }: {
  machineId: string;
  metricsKey: string;
  coreIdsKey?: string;
  range: RangeId;
  enabled?: boolean;
  autoRefresh?: boolean;
  refreshNonce?: number;
}) {
  const [settled, setSettled] = useState({machineId, metricsKey, coreIdsKey, range});
  const sameScope = settled.machineId === machineId && settled.range === range;
  useEffect(() => {
    if (!sameScope) {
      setSettled({machineId, metricsKey, coreIdsKey, range});
      return;
    }
    const timer = window.setTimeout(() => setSettled({machineId, metricsKey, coreIdsKey, range}), 300);
    return () => window.clearTimeout(timer);
  }, [machineId, metricsKey, coreIdsKey, range, sameScope]);
  // Route/range changes take effect immediately; rapid metric/core changes
  // within the same view coalesce before spending the session's query budget.
  const selection = sameScope ? settled : {machineId, metricsKey, coreIdsKey, range};
  const queryMetricsKey = selection.metricsKey;
  const queryCoreIdsKey = selection.coreIdsKey;
  const key = `${machineId}|${queryMetricsKey}|${range}|${queryCoreIdsKey ?? "all"}`;
  const [data, setData] = useState<LoadedHistory | null>(null);
  const [failure, setFailure] = useState<{ key: string; error: HistoryError } | null>(null);
  const [loadingRequest, setLoadingRequest] = useState<{ key: string; manual: boolean } | null>(null);
  const previousRefreshNonce = useRef(refreshNonce);

  useEffect(() => {
    const manualRefresh = previousRefreshNonce.current !== refreshNonce;
    previousRefreshNonce.current = refreshNonce;
    if (!enabled || !machineId || !queryMetricsKey) return;
    let active = true;
    let pending = false;
    let limited = false;
    let lastStartedAt = -Infinity;
    const controller = new AbortController();
    const interval = refreshIntervalMs(range);
    const load = async (manual = false) => {
      if (!active || pending || limited) return;
      pending = true;
      lastStartedAt = Date.now();
      setLoadingRequest({ key, manual });
      try {
        const response = await fetch(historyRequestUrl(machineId, queryMetricsKey.split(","), range, Date.now(), 600,
          queryCoreIdsKey === undefined ? undefined : queryCoreIdsKey.split(",").filter(Boolean)), {
          signal: requestSignal(controller), cache: "no-store", headers: { Accept: "application/json" },
        });
        if (!response.ok) throw await responseError(response);
        const result = parseHistoryResult(await response.json());
        if (result === null || result.machineId !== machineId) {
          throw { kind: "invalid", message: "Unexpected history response." } satisfies HistoryError;
        }
        if (!active) return;
        setData({ key, range, result, loadedAt: Date.now() });
        setFailure(null);
      } catch (reason) {
        if (!active || controller.signal.aborted) return;
        const error: HistoryError = isHistoryError(reason) ? reason : { kind: "network", message: "Could not reach the hub. Try again." };
        limited = error.kind === "rate-limited";
        // Authorization loss must remove the retained result as well.
        if (["unauthorized", "denied", "missing"].includes(error.kind)) setData(null);
        setFailure({ key, error });
      } finally {
        pending = false;
        if (active) setLoadingRequest(null);
      }
    };
    const tick = () => {
      if (document.visibilityState !== "visible" || Date.now() - lastStartedAt < interval) return;
      void load();
    };
    void load(manualRefresh);
    const timer = autoRefresh ? window.setInterval(tick, interval) : null;
    if (autoRefresh) document.addEventListener("visibilitychange", tick);
    return () => {
      active = false;
      controller.abort();
      if (timer !== null) window.clearInterval(timer);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [enabled, machineId, queryMetricsKey, queryCoreIdsKey, range, key, autoRefresh, refreshNonce]);

  return {
    data: enabled && data?.result.machineId === machineId && data.range === range ? data : null,
    error: enabled && failure?.key === key ? failure.error : null,
    loading: enabled && loadingRequest?.key === key,
    manualRefreshing: enabled && loadingRequest?.key === key && loadingRequest.manual,
  };
}
