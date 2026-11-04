/**
 * One independently owned Hub connection.
 *
 * A HubConnection owns everything that varies per Hub connection: its local
 * sharing policy, its effective delivery cadence, its durable outbox
 * directory (including the previous-endpoint upgrade location), recovered
 * queue sanitation, inventory selection and version refresh, and the durable
 * append-before-send submission path. The one runtime caller only fans a
 * shared sample out to every connection and hands over nothing else; filter,
 * append, notify-delivery and inventory choreography are internal
 * obligations now.
 *
 * What stays at Collector scope: the shared sampling loop and sampler, the
 * GPU poller, the global disk budget instance, and cross-Hub detail
 * scheduling, which binds per connection through `detailPolicy`, `sendDetail`
 * and `attachDetails`. Detail data stays request-scoped: it is never queued
 * in the outbox, and its subscriptions are cancelled when this connection
 * disconnects.
 *
 * A connection's failures never affect sampling or another connection: the
 * outbox absorbs storage failures, the session owns its own socket, timers
 * and reconnect schedule, and `acceptSample` never throws outward.
 */
import { PREVIOUS_CONNECT_PATH, type DestinationEndpoint } from '../config';
import type { CollectorIdentity } from '../identity';
import type {
  CollectionFailure,
  DetailCancel,
  DetailEnded,
  DetailResult,
  DetailSubscribe,
  Inventory,
  InventoryMessage,
  MetricsRecord,
  StatusMessage,
} from '../../src/contracts';
import {
  applySharing,
  createMetricsFilter,
  effectiveIntervalMs,
  filterInventory,
  normalizeSharing,
  type SharingPolicy,
} from './policy';
import { DestinationOutbox, outboxDirFor } from './outbox';
import type { OutboxBudget } from './budget';
import { DestinationSession, type SessionState, type SocketFactory } from './session';
import type { BackoffOptions } from './backoff';

/** The shared sampler's read-only surface, supplied by the Collector loop. */
export type SharedSampler = {
  inventory(): Inventory;
  inventoryVersion(): number;
  failures(): CollectionFailure[];
};

/** Cross-Hub detail scheduling bound to exactly this connection. */
export type HubDetailRouting = {
  handle(message: DetailSubscribe | DetailCancel): void;
  connectionDisconnected(): void;
};

export type HubConnectionOptions = {
  /** Destination number for logs and cross-Hub detail scheduling. */
  index: number;
  endpoint: DestinationEndpoint;
  /** Shared sampling cadence (milliseconds). */
  baseIntervalMs: number;
  /** Collector state directory that holds this connection's outbox. */
  stateDir: string;
  identity: CollectorIdentity;
  /** Shared aggregate disk budget for all of the collector's connections. */
  budget: OutboxBudget;
  /** Allocates sequence numbers for locally created gap records. */
  allocateSequence: () => number;
  /** The shared sampler's read-only surface. */
  sampler: SharedSampler;
  collectorVersion: string;
  socketFactory: SocketFactory;
  onEvent?: (line: string) => void;
  /** Socket schedule tuning; production uses the session defaults. */
  backoff?: BackoffOptions;
  rand?: () => number;
};

export class HubConnection {
  private readonly index: number;
  private readonly policy: SharingPolicy;
  private readonly sampleFilter: (record: MetricsRecord, monotonicMs: number) => MetricsRecord | null;
  private readonly sampler: SharedSampler;
  private readonly outbox: DestinationOutbox;
  private readonly session: DestinationSession;
  private readonly onEvent: (line: string) => void;

  private details: HubDetailRouting | null = null;
  /** Inventory version last confirmed sent on the current socket generation. */
  private sentInventoryVersion = -1;

  private constructor(options: HubConnectionOptions, outbox: DestinationOutbox) {
    this.index = options.index;
    this.policy = normalizeSharing(options.endpoint.sharing);
    this.sampleFilter = createMetricsFilter(options.endpoint, options.baseIntervalMs);
    this.sampler = options.sampler;
    this.outbox = outbox;
    this.onEvent = options.onEvent ?? (() => {});

    const capabilities: Array<'metrics' | 'inventory' | 'processes'> = ['metrics', 'inventory'];
    if (this.policy.processes) capabilities.push('processes');

    this.session = new DestinationSession({
      name: `hub${options.index}`,
      url: options.endpoint.url,
      token: options.endpoint.token,
      identity: options.identity,
      outbox,
      expectedIntervalMs: effectiveIntervalMs(options.baseIntervalMs, options.endpoint.intervalMs),
      capabilities,
      collectorVersion: options.collectorVersion,
      socketFactory: options.socketFactory,
      backoff: options.backoff,
      rand: options.rand,
      onEvent: line => this.onEvent(line),
      onDetailRequest: message => this.details?.handle(message),
      onDisconnect: () => this.details?.connectionDisconnected(),
      // The session announces inventory after every welcome; here it always
      // pulls the current snapshot, filtered for this connection's policy.
      inventoryProvider: () => this.buildInventory(),
    });
  }

  /**
   * Opens this connection's durable outbox under its current endpoint URL,
   * upgrades outboxes that predate the endpoint consolidation, and re-applies
   * the privacy policy to recovered pending records so none of them can
   * leave with forbidden fields. Sanitation never cadence-filters: retained
   * samples were already cadence-selected when they were captured.
   */
  static open(options: HubConnectionOptions): HubConnection {
    const dir = outboxDirFor(options.stateDir, options.endpoint.url);
    const previousUrl = new URL(options.endpoint.url);
    previousUrl.pathname = PREVIOUS_CONNECT_PATH;
    const outbox = DestinationOutbox.open({
      dir,
      previousDir: outboxDirFor(options.stateDir, previousUrl.href),
      budget: options.budget,
      allocateSequence: options.allocateSequence,
      identity: {
        collectorId: options.identity.collectorId,
        runId: options.identity.runId,
        bootId: options.identity.bootId,
      },
    });
    const connection = new HubConnection(options, outbox);
    try {
      outbox.filterPending(record => applySharing(record, connection.policy));
    } catch (error) {
      outbox.close();
      throw error;
    }
    return connection;
  }

  /** Opens the socket and begins durable delivery. Detail routing must be attached first. */
  start(): void {
    this.session.start();
  }

  /** Closes the socket and the outbox; pending authorized delivery stays durable. */
  stop(): void {
    this.session.stop();
    this.outbox.close();
  }

  /**
   * Admits one shared sample to this connection. Applies the delivery
   * cadence and privacy policy before anything is queued, then delivers the
   * appended record (append-before-send), then refreshes the inventory when
   * its version has moved. Never throws: a connection's failures must not
   * block sampling or another connection.
   */
  acceptSample(record: MetricsRecord, monotonicMs: number): void {
    try {
      const filtered = this.sampleFilter(record, monotonicMs);
      if (filtered) {
        this.outbox.append(filtered);
        this.session.deliverLive();
      }
      this.refreshInventory();
    } catch {
      this.log('destination delivery failed');
    }
  }

  /** Periodic status snapshot from this connection's own delivery state. Never throws. */
  refreshStatus(): boolean {
    try {
      const message: StatusMessage = {
        type: 'status',
        capturedAt: new Date().toISOString(),
        delivery: this.outbox.status(),
        failures: this.sampler.failures(),
      };
      return this.session.sendStatus(message);
    } catch {
      this.log('status delivery failed');
      return false;
    }
  }

  /** This connection's local authorization for cross-Hub detail scheduling. */
  detailPolicy(): { processes: boolean; processArguments: boolean } {
    return { processes: this.policy.processes, processArguments: this.policy.processArguments };
  }

  /** Sends request-scoped detail; it is never queued in the durable outbox. */
  sendDetail(message: DetailResult | DetailEnded): boolean {
    return this.session.sendDetail(message);
  }

  /**
   * Binds cross-Hub detail scheduling. Inbound subscription requests route
   * to `handle`; this connection's disconnects cancel its subscriptions.
   */
  attachDetails(details: HubDetailRouting): void {
    this.details = details;
  }

  /** Observable session state for tests and operational logging. */
  describeState(): SessionState {
    return this.session.describeState();
  }

  private refreshInventory(): void {
    const version = this.sampler.inventoryVersion();
    if (version === this.sentInventoryVersion) return;
    if (this.session.sendInventory(this.buildInventory())) this.sentInventoryVersion = version;
  }

  private buildInventory(): InventoryMessage {
    return {
      type: 'inventory',
      capturedAt: new Date().toISOString(),
      inventory: filterInventory(this.sampler.inventory(), this.policy),
    };
  }

  private log(line: string): void {
    try {
      this.onEvent(`hub${this.index}: ${line}`);
    } catch {
      // A misbehaving log sink must not break delivery.
    }
  }
}
