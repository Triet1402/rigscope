/**
 * A thin EventSource controller for the hub's live feed (GET /api/live).
 *
 * Design rules from the milestone 3 agreement and ARCHITECTURE.md:
 * - At most one EventSource per tab; reopening always closes the previous
 *   source first ("no repeated streams").
 * - Errors close the source immediately and hand control to the caller.
 *   EventSource's own automatic retry loop is not used, because it cannot
 *   distinguish session expiry from an unreachable hub and would retry
 *   forever against a 401.
 * - `snapshot`, `update`, and `reset` all carry a full HubSnapshot; the
 *   controller parses and validates the frame envelope only.
 *
 * Browser-only module (uses the EventSource global); imported by the
 * client provider, never by Node code.
 */

import { parseHubSnapshot, type HubSnapshot } from "../contracts";

export type LiveStreamEvent = "snapshot" | "update" | "reset";

export type LiveStreamHandlers = {
  onSnapshot: (snapshot: HubSnapshot, event: LiveStreamEvent, eventId: string) => void;
  onError: () => void;
};

const LIVE_EVENTS: readonly LiveStreamEvent[] = ["snapshot", "update", "reset"];

export class LiveStream {
  private source: EventSource | null = null;
  private lastId: string | null = null;

  get lastEventId(): string | null {
    return this.lastId;
  }

  /** True while the source exists and has not been closed. */
  get isOpen(): boolean {
    return this.source !== null && this.source.readyState !== 2; // 2 = CLOSED
  }

  /**
   * Opens a stream for one URL, replacing any previous source. Snapshot
   * handlers are invoked only for frames served by this source; events
   * that arrive after close are ignored.
   */
  start(url: string, handlers: LiveStreamHandlers): void {
    this.close();
    const source = new EventSource(url);
    this.source = source;

    const onEvent = (event: LiveStreamEvent) => (raw: Event) => {
      if (this.source !== source) return; // a superseded/closed source
      if (this.source === null) return;
      const message = raw as MessageEvent<string>;
      const eventId = message.lastEventId;
      if (typeof eventId === "string" && eventId.length > 0) {
        this.lastId = eventId;
      }
      try {
        handlers.onSnapshot(parseHubSnapshot(JSON.parse(message.data)), event, eventId ?? "");
      } catch {
        // A malformed feed is unavailable, even if its socket stays open.
        this.close();
        handlers.onError();
      }
    };

    for (const name of LIVE_EVENTS) {
      source.addEventListener(name, onEvent(name) as EventListener);
    }
    source.onerror = () => {
      if (this.source !== source) return;
      // Close synchronously: EventSource would otherwise begin its own
      // unbounded reconnect loop, losing control over probing and backoff.
      this.close();
      handlers.onError();
    };
  }

  /** Closes the EventSource, if any. Safe to call repeatedly. */
  close(): void {
    this.source?.close();
    this.source = null;
  }
}
