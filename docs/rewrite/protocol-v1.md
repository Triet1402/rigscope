# Rigscope wire protocol v1

Status: current. Source of truth: `internal/protocol` (Go). The browser-safe
TypeScript parsers in `src/contracts/telemetry.ts` implement the same contract
for the UI, and golden fixtures in `testdata/protocol/` are shared by both test
suites.

This document describes the contract between one collector and one hub over the
collector WebSocket. It does not describe the browser API, storage, alerting or
the hub's internal HTTP endpoints.

## 1. Scope and conventions

- One JSON object per WebSocket text frame. Binary frames are not part of the
  contract.
- Parsers are strict:
  - unknown fields are rejected, never ignored or stripped;
  - required fields must be present (a JSON `null` is present, then fails its
    type check);
  - enum values are closed sets;
  - array lengths, string lengths and numeric ranges are bounded;
  - integers must be JavaScript-safe integers (`-2^53+1 .. 2^53-1`);
  - numbers must be finite; `NaN` and infinities are not valid JSON anyway;
  - timestamps use one strict grammar in both languages:
    `YYYY-MM-DDTHH:MM:SS(.fraction of 1-9 digits)?(Z|±HH:MM)`, with valid
    calendar and clock ranges, an offset within ±23:59, and values inside
    `2000-01-01T00:00:00Z` .. `2200-01-01T00:00:00Z`. Fraction digits beyond
    milliseconds are truncated, so ordering and the window use millisecond
    precision. Legacy `Date.parse` forms (missing seconds, comma fractions,
    RFC 1123, `+24:00`) are rejected. Collectors emit the canonical
    `toISOString()` form (`2026-11-12T10:00:00.000Z`) and encoders preserve the
    wire string exactly.
- String length is measured in UTF-16 code units, matching JavaScript's
  `String.length`. ASCII is one unit per byte.
- Every string value must be a well-formed Unicode scalar sequence. A lone
  UTF-16 surrogate escape such as `"\ud800"` is rejected in both languages
  rather than silently replaced with U+FFFD, so identity strings cannot change
  under a round trip.
- Numerically integral JSON numbers written as `1.0` or `1e3` are accepted where
  an integer is required; `1.5` is not.
- **Never a fabricated zero.** Optional readings are absent when the collector
  cannot measure them, or an explicit `null` when the reading was attempted and
  failed. A metric family that is unsupported or failed carries
  `status: "unavailable"` and a reason, and no reading fields.
- Duplicate JSON object keys are not detectable through `JSON.parse`; the last
  value wins in both implementations. Messages are generated, not hand-edited.

### Limits

| Constant | Value | Meaning |
| --- | --- | --- |
| `MaxMessageBytes` | 262144 | largest message of any type except `detail.result` |
| `MaxDetailBytes` | 1048576 | largest `detail.result` |
| `MaxBatchRecords` | 32 | records in one `samples` message |
| `MaxInFlightBatches` | 64 | largest unacknowledged batch window a hub may advertise |
| `MaxPersistentFlushMS` | 5000 | shared hub flush bound (delivery policy) |
| `MaxCores` | 1024 | `cpu.perCore` entries and inventory logical cores |
| `MaxGPUs` | 16 | GPU readings and inventory entries |
| `MaxFilesystems` | 64 | filesystem readings and inventory entries |
| `MaxDisks` | 64 | device readings and inventory entries |
| `MaxInterfaces` | 64 | interface readings and inventory entries |
| `MaxAddressesPerInterface` | 32 | addresses on one inventory interface |
| `MaxProcessRows` | 6000 | rows in one process snapshot |
| `MaxCollectionFailures` | 32 | failures in one `status` message |
| `MinDetailIntervalMS` / `MaxDetailIntervalMS` | 1000 / 60000 | detail sampling interval |
| `MinDetailLeaseMS` / `MaxDetailLeaseMS` | 5000 / 120000 | detail lease |
| `MinExpectedIntervalMS` / `MaxExpectedIntervalMS` | 100 / 7200000 | advertised delivery cadence |
| id strings | 1..128 | `collectorId`, `runId`, `hubRunId`, `machineId`, `subscriptionId` |
| resource ids | 1..256 | stable ids such as `gpu:GPU-…`, `fs:/`, `disk:sda`, `net:eth0` |
| text | 0..256 | reasons, names, labels |
| hostname | 0..255 | |
| timestamp | 1..64 chars | |

`runId` changes when the collector process restarts. `bootId` changes when the
OS boots. `(collectorId, runId, sequence)` identifies a record; `sequence`
increases across record kinds within a run and may skip values.

## 2. Versioning

`hello.protocol` (collector → hub) is the collector's **highest supported
version**. `welcome.protocol` (hub → collector) is the **version the hub selected
for this session**. Both are optional positive integers; `Version = 1` is the
current version.

- **A missing marker means version 1**: the unversioned, 0.2-compatible
  contract. New collectors always send `hello.protocol`; absence is tolerated
  only for older peers.
- The hub supports the current version and the previous one, and encodes and
  validates each session with that session's version. With `Version = 1` the
  accepted set is exactly `{1}`; the window widens automatically as later
  versions ship.
- `hello.protocol` is the collector's highest version, not a demand: a hub may
  select an older supported version and answer with it in `welcome.protocol`.
  A collector that receives a `welcome.protocol` it does not support must close
  the connection and log an upgrade hint.
- An unsupported version is a policy failure, not a parse failure. The v1
  `welcome` schema is success-only, so a typed `welcome` error carrying the
  supported range is a **planned extension**, not part of v1. Until it ships,
  the hub closes with an application close code and a human-readable reason and
  shows such collectors as "needs upgrade". Collectors must not retry the same
  version in a tight loop.
- **Upgrade order is hub first, then collectors.** Compatibility is tested in
  both directions (old collector → new hub, new collector → old hub refused
  cleanly).

The TypeScript prototype was unversioned. `protocol` was added to its parsers
so the Go collector could talk to it during the transition; the Go hub now
speaks version 1 and the prototype is gone.

## 3. Transport

- Endpoint: `GET /api/collector/connect` upgraded to WebSocket (WSS).
- TLS is required for anything that is not loopback. Certificate trust is the
  ordinary system trust store; credentials go in an `Authorization` header, not
  in the URL.
- Authentication happens before the WebSocket upgrade or immediately after it,
  depending on the server implementation. A missing or invalid credential is
  rejected with HTTP 401; a disabled or unauthorized machine gets 403; a full
  connection budget gets 503.
- The first collector message on an active connection must be `hello`, within
  `HELLO_TIMEOUT_MS = 5000`. Any other first message is a protocol-step error.
- A `hello.collectorId` that does not match the authenticated identity is an
  identity-mismatch error. A second `hello` on the same connection is a
  protocol-step error.
- Liveness uses WebSocket ping frames every 15 s
  (`COLLECTOR_PING_INTERVAL_MS`). A connection with no activity for 45 s
  (`COLLECTOR_IDLE_TIMEOUT_MS`) is closed.
- The hub bounds frame sizes, message rate, output queue and connection count.
  The prototype defaults: 20 messages/s sustained with a burst of 30, a 1 MiB
  send buffer, 100 concurrent collector connections.
- Close codes used by the prototype hub:

| Code | Meaning |
| --- | --- |
| 1001 | hub shutting down |
| 1008 | policy/authentication failure |
| 1009 | message too large |
| 4001 | hello deadline exceeded |
| 4002 | hello identity mismatch |
| 4003 | invalid message (JSON or contract) |
| 4005 | session conflict (same collector, conflicting run) |
| 4006 | protocol step violation (first message, duplicate hello) |
| 4007 | message rate limit exceeded |

The hub terminates a superseded connection for the same collector when a newer
authenticated run starts. Reconnection uses exponential backoff with jitter,
outside this contract.

## 4. Collector → hub messages

All messages are objects with a `type` discriminator. Messages are shown with
their exact wire field names.

### 4.1 `hello`

Opens a session and advertises identity and cadence.

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"hello"` |
| `collectorId` | string | 1..128 |
| `runId` | string | 1..128, new on collector restart |
| `bootId` | string | 0..128, identifies the OS boot |
| `hostname` | string | 0..255 |
| `capabilities` | array | 1..3 entries, unique, must include `"metrics"` |
| `expectedIntervalMs` | integer | 100..7200000; nominal delivery cadence for this destination |
| `protocol` | integer | optional; positive; see §2 |

Capability names: `metrics`, `inventory`, `processes`.

### 4.2 `samples`

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"samples"` |
| `mode` | string | `"live"` or `"replay"` |
| `records` | array | 1..32 records, unique by `(runId, sequence)` within the batch |

A record is a metrics record or a gap record.

Common envelope:

| Field | Type | Rules |
| --- | --- | --- |
| `collectorId` | string | 1..128 |
| `runId` | string | 1..128 |
| `bootId` | string | 0..128 |
| `sequence` | integer | 0..2^53-1, increasing within a run |
| `capturedAt` | timestamp | collector clock at capture |
| `kind` | string | `"metrics"` or `"gap"` |
| `payload` | object | kind-specific |

#### Metrics record (`kind: "metrics"`)

`payload` has exactly the six metric families. Every family is present.

| Family | Shape |
| --- | --- |
| `cpu` | available: `{status:"ok", usagePercent, perCore, temperatureC?, powerWatts?, iowaitPercent?, pressureSomePercent?}` or `{status:"unavailable", reason}` |
| `memory` | available: `{status:"ok", usedBytes, totalBytes, pressureSomePercent?, pressureFullPercent?}` or unavailable |
| `swap` | available: `{status:"ok", usedBytes, totalBytes}` or unavailable |
| `gpus` | available: `{status:"ok", devices:[GpuReading]}` or unavailable |
| `disks` | available: `{status:"ok", filesystems:[FilesystemReading], devices:[DiskIoReading], pressureSomePercent?, pressureFullPercent?}` or unavailable |
| `network` | available: `{status:"ok", interfaces:[InterfaceReading]}` or unavailable |

CPU reading:

| Field | Type | Rules |
| --- | --- | --- |
| `status` | string | `"ok"` |
| `usagePercent` | number | 0..100, busy-counter delta over monotonic elapsed × logical cores |
| `perCore` | array | 0..1024 entries; each entry is a number 0..100 or `null` when that core's counters failed |
| `temperatureC` | number, null, or absent | -50..200; package/Tctl/Tdie/core temperature |
| `powerWatts` | number, null, or absent | 0..10000; top-level RAPL package power |
| `iowaitPercent` | number, null, or absent | 0..100 |
| `pressureSomePercent` | number, null, or absent | 0..100; PSI cpu `some` avg10 |

Optional-nullable fields are: absent when the metric is not applicable, `null`
when reading was attempted and failed, otherwise the value. The distinction is
preserved by the Go `Optional[T]` type.

Memory reading: `usedBytes` and `totalBytes` are integers 0..2^53-1 with
`usedBytes <= totalBytes`; PSI fields are optional-nullable numbers 0..100.

Swap reading: `usedBytes` and `totalBytes` are integers with
`usedBytes <= totalBytes`. Swap has no pressure fields.

GPU reading:

| Field | Type | Rules |
| --- | --- | --- |
| `id` | string | 1..256, stable (NVIDIA UUID, PCI address), unique in the list |
| `status` | string | `"ok"` or `"unavailable"` |
| `utilizationPercent` | number or null | 0..100 |
| `memoryUsedBytes` | integer or null | 0..2^53-1 |
| `memoryTotalBytes` | integer or null | 0..2^53-1, must be >= used when both are numbers |
| `temperatureC` | number or null | -50..200 |
| `powerWatts` | number or null | 0..10000 |
| `junctionTemperatureC` | number, null, or absent | -50..200; GPU register hotspot |
| `vramTemperatureC` | number, null, or absent | -50..200; GPU register VRAM |

An unavailable GPU reading is `{id, status:"unavailable", reason}` with no
reading fields.

Filesystem reading:

| Field | Type | Rules |
| --- | --- | --- |
| `id` | string | 1..256, unique in the list |
| `status` | string | `"ok"` or `"unavailable"` |
| `usedBytes` / `totalBytes` | integer | 0..2^53-1, `usedBytes <= totalBytes` |
| `inodesUsed` | integer, null, or absent | 0..2^53-1 |
| `inodesTotal` | integer, null, or absent | 0..2^53-1, `inodesUsed <= inodesTotal` when both are values |

Disk I/O reading:

| Field | Type | Rules |
| --- | --- | --- |
| `id` | string | 1..256, unique in the list |
| `status` | string | `"ok"` or `"unavailable"` |
| `readBytesPerSec` / `writeBytesPerSec` | number | 0..1e13 |
| `readLatencyMs` | number, null, or absent | 0..86400000; null when there were no reads |
| `writeLatencyMs` | number, null, or absent | 0..86400000; null when there were no writes |
| `ioUtilizationPercent` | number, null, or absent | 0..100 |

Interface reading: `id` (1..256, unique), `status`, and when available
`rxBytesPerSec` / `txBytesPerSec` (numbers 0..1e13).

#### Gap record (`kind: "gap"`)

Reports an interval whose records were dropped by the destination.

| Field | Type | Rules |
| --- | --- | --- |
| `fromCapturedAt` | timestamp | capture time of the first dropped record, inclusive |
| `toCapturedAt` | timestamp | capture time of the last dropped record, inclusive, not before `fromCapturedAt` |
| `droppedRecords` | integer | 1..2^53-1 |
| `reason` | string | `outbox-age`, `outbox-bytes`, `disk-budget`, `write-failure`, `rejected` |

### 4.3 `inventory`

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"inventory"` |
| `capturedAt` | timestamp | |
| `inventory` | object | see below |

Inventory:

| Field | Type | Rules |
| --- | --- | --- |
| `os` | object | `platform` (0..64), `release` (0..256), `distro` (0..256 or null), `arch` (0..64) |
| `cpu` | object | `model` (0..256 or null), `logicalCores` (integer 0..1024) |
| `memory` | object | `totalBytes` (integer or null), `swapTotalBytes` (integer or null) |
| `gpus` | array | 0..16 GPU inventory entries, unique ids |
| `filesystems` | array | 0..64 entries, unique ids |
| `disks` | array | 0..64 entries, unique ids |
| `network` | array | 0..64 entries, unique ids |
| `capabilities` | object | exactly `cpu`, `memory`, `swap`, `gpus`, `disks`, `network`, `processes` |

GPU inventory entry:

| Field | Type | Rules |
| --- | --- | --- |
| `id` | string | 1..256 |
| `name` | string | 0..256 |
| `vendor` | string | `nvidia`, `amd`, `intel`, `aspeed`, `other` |
| `pciBusId` | string or null | 1..64 |
| `telemetry` | string | `supported` or `unsupported` (management adapters) |
| `memoryTotalBytes` | integer or null | 0..2^53-1 |
| `reason` | string, optional | 0..256 |

Filesystem inventory entry: `id` (1..256), `mountPoint` (1..256), `device`
(0..256), `fsType` (0..64), `totalBytes` (integer or null).

Disk inventory entry: `id` (1..256), `name` (1..64), `model` (0..256 or null),
`rotational` (boolean or null), `sizeBytes` (integer or null).

Interface inventory entry: `id` (1..256), `name` (1..64), `mac` (1..64 or
null), `addresses` (0..32), `virtual` (boolean), `state` (`up`, `down`,
`unknown`), `speedMbps` (integer 0..10000000 or null).

Address entry: `family` (`ipv4` or `ipv6`), `address` (2..64), `prefix`
(integer 0..32 for IPv4, 0..128 for IPv6).

Capability state: `{status, reason?}` where `status` is `supported`,
`unsupported` or `withheld` (withheld means the local sharing policy for this
hub), and `reason` is an optional 0..256 string.

### 4.4 `status`

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"status"` |
| `capturedAt` | timestamp | |
| `delivery` | object | see below |
| `failures` | array | 0..32 collection failures |

Delivery status:

| Field | Type | Rules |
| --- | --- | --- |
| `queueMode` | string, optional | `memory` or `durable`; absent for collectors predating the field. `durable` is legacy 0.2 behaviour the wire still tolerates; 1.0 collectors report `memory`. |
| `pendingRecords` | integer | 0..2^53-1 |
| `pendingBytes` | integer | 0..2^53-1 |
| `oldestPendingCapturedAt` | timestamp or null | |
| `inFlightRecords` | integer | 0..2^53-1 |
| `droppedRecords` | integer | 0..2^53-1 |
| `replaying` | boolean | |
| `lastAckAt` | timestamp or null | |

Collection failure: `metric` (string 1..64), `resourceId` (string 1..256 or
null), `reason` (string 0..256). `resourceId` is `null` for failures that are
not tied to one resource.

### 4.5 `detail.result`

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"detail.result"` |
| `subscriptionId` | string | 1..128 |
| `capturedAt` | timestamp | |
| `snapshot` | object | see below |

Snapshot: `totalProcesses` (integer 0..10000000), `truncated` (boolean),
`inaccessible` (integer 0..10000000), `processes` (0..6000 rows).

Process row:

| Field | Type | Rules |
| --- | --- | --- |
| `pid` / `ppid` | integer | 0..2^31 |
| `startTicks` | integer | 0..2^53-1; with `pid` identifies a process |
| `name` | string | 0..256 |
| `user` | string or null | 0..64 |
| `state` | string | 0..16 |
| `cpuPercent` | number or null | 0..102400 (100 = one full core) |
| `memoryRssBytes` | integer or null | 0..2^53-1 |
| `gpuMemoryBytes` | integer or null | 0..2^53-1 |
| `ioReadBytesPerSec` / `ioWriteBytesPerSec` | number or null | 0..1e13 |
| `threads` | integer or null | 0..10000000 |
| `unit` | string or null | 1..256; systemd unit/scope |
| `command` | string, optional | 0..4096; present only when the destination policy allows arguments |

### 4.6 `detail.ended`

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"detail.ended"` |
| `subscriptionId` | string | 1..128 |
| `reason` | string | `expired`, `cancelled`, `withheld`, `unsupported`, `busy`, `failed` |

## 5. Hub → collector messages

### 5.1 `welcome`

Answers a valid `hello`.

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"welcome"` |
| `hubRunId` | string | 1..128 |
| `machineId` | string | 1..128; hub-local machine identity |
| `delivery` | string | `durable` (ack follows a crash-safe commit) or `memory` (ack follows RAM acceptance; history intentionally lost on hub restart) |
| `maxInFlightBatches` | integer | 1..64 |
| `protocol` | integer, optional | positive; the accepted protocol version (§2) |

### 5.2 `ack`

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"ack"` |
| `accepted` | array | 0..2048 `RecordRef` entries |
| `rejected` | array | 0..2048 rejected entries |
| `retryAfterMs` | integer, optional | 0..600000 |

`RecordRef` is `{runId: string 1..128, sequence: integer 0..2^53-1}`. A rejected
entry is `{runId, sequence, reason}` where `reason` is 0..256.

`accepted` and `rejected` are independent lists; a record appears in at most one
of them. Duplicates of already committed records are reported as accepted.
`rejected` entries must never be retried by the collector.

### 5.3 `detail.subscribe`

Create-or-renew: re-sending an active subscription id renews its lease.

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"detail.subscribe"` |
| `subscriptionId` | string | 1..128 |
| `kind` | string | `"processes"` (only kind) |
| `intervalMs` | integer | 1000..60000 |
| `leaseMs` | integer | 5000..120000 |

### 5.4 `detail.cancel`

| Field | Type | Rules |
| --- | --- | --- |
| `type` | string | `"detail.cancel"` |
| `subscriptionId` | string | 1..128 |

## 6. Delivery semantics

### Acknowledgement

- The hub acknowledges records only after they are durable under its configured
  delivery mode. In `durable` mode that is a crash-safe commit; in `memory` mode
  that is acceptance into RAM, and history intentionally disappears on hub
  restart.
- The collector retries retained records until they appear in `accepted` or
  `rejected`.
- Retries are deduplicated by `(collectorId, runId, sequence)`. The hub reports
  duplicates of committed records in `accepted`.
- `rejected` means the record can never succeed (contract violation, retention
  policy) and must be dropped, not retried.
- When the hub cannot accept a batch now it may answer with an ack containing
  `retryAfterMs`; the collector waits at least that long before retrying.
- `maxInFlightBatches` in `welcome` bounds the unacknowledged sample batches a
  collector may keep outstanding. The protocol ceiling is 64; a welcome above
  it is rejected by collectors.

### Live versus replay

- `mode: "live"` batches may update latest state and open incidents.
- `mode: "replay"` batches only fill history. They must never update live
  state, appear fresh, or trigger incidents. Backfill never supersedes newer
  live state.
- Ordering is guaranteed only within one connection; after a reconnect the
  collector resumes from its own bounded queue state. Pending records not
  drained before a restart are lost and reported as unknown loss (see
  "Secrets and retention").

### Gaps

- When a destination drops records it can describe (queue overflow, age/byte
  budgets), the collector emits a `gap` record covering the dropped capture
  interval. `outbox-age` and `outbox-bytes` remain valid wire reason names for
  compatibility even though 1.0 collectors use a RAM queue.
- A gap is a record: it occupies a sequence number and is acknowledged like a
  sample.
- Gaps are coalesced within the retention budget; `droppedRecords` counts the
  dropped records the gap represents.
- A gap always describes a **known** interval: `fromCapturedAt`, `toCapturedAt`
  and `droppedRecords` are facts the collector observed. The 1.0 RAM queue
  emits `outbox-age` (age cap) and `outbox-bytes` (byte cap or a single record
  larger than the whole queue). `rejected`, `write-failure` and `disk-budget`
  remain valid wire values for older collectors and future failure paths; a
  permanently rejected record is removed and logged, and is not re-described
  as a fabricated gap.

### Unknown loss after restart

A crash or restart discards the RAM queue. The collector then cannot know the
first lost capture time or how many records were lost (and a `gap` with invented
values would be a fabricated fact), so it **emits no gap record** for that
interval.

The explicit wire marker is the **run boundary**: `runId` is new on every
collector launch, and every record carries `(collectorId, runId, sequence)`.
The hub observes a new `runId` in `hello` and later records from that run, so
the interval between the previous run's last record and the new run's first
record is unaccounted rather than silently "zero samples".

Both hub history modes now treat that boundary explicitly. When two consecutive
stored samples of one machine carry different `runId` values, the hub reports
an interval of kind `missing` with reason `collector restarted`, covering the
previous run's last capture time through the new run's first capture time. It
carries **no** `droppedRecords` count, because the collector could not observe
that interval. Charts segment at every recorded gap, so a line or area never
connects across a collector restart even when the restart was shorter than the
normal missing-sample threshold. Consumers must treat that interval as unknown
loss, not as a reported `gap` record and not as a healthy zero.

The collector also reports its own view honestly: `delivery.droppedRecords`
counts only drops it knows about, and the queue is empty after a restart rather
than pretending the previous pending records were delivered. A graceful
shutdown drains and reports known drops before exiting, so run-boundary unknown
loss is the crash/forced-stop path, not the normal one.

### Secrets and retention

- Process arguments and environment variables are omitted by default. The
  `command` field appears only when the destination's policy allows arguments.
- Process detail is transient and request-scoped. It is not persisted or spooled
  by default.
- The 0.2 collector's durable SQLite outbox is **legacy behaviour**: the wire
  still tolerates `queueMode: "durable"` and the hub accepts it, but 1.0
  collectors use **one bounded RAM queue**. New collectors report
  `queueMode: "memory"` (or omit it when talking to an older hub).
- Shutdown performs a bounded graceful drain. Records that cannot be delivered
  within the drain are dropped by design, never silently retained.
- Every **known** drop the queue can still report (queue overflow, age/byte
  budget) emits a `gap` record covering the dropped interval, as described
  under "Gaps". A permanent rejection is dropped and logged; the hub already
  holds the rejection reason. A drain timeout that leaves no usable connection
  cannot deliver its own gap; that residual loss is bounded by the drain
  deadline and, across a restart, becomes run-boundary unknown loss.
- A crash or restart discards the pending RAM queue, so the collector cannot
  know the exact dropped interval. It must not fabricate a gap: the new `runId`
  in `hello` is the explicit unknown-loss marker (see "Unknown loss after
  restart"). `delivery.droppedRecords` never counts an unknown interval.

## 7. Detail subscriptions

- A browser viewing process detail causes the hub to send `detail.subscribe` over
  the collector's existing WebSocket.
- The subscription is a lease: the hub renews it before `leaseMs` elapses, and
  the collector stops sampling when the lease expires without renewal. The
  collector must answer with `detail.result` at approximately `intervalMs`, or
  with `detail.ended` explaining why it cannot.
- Re-sending the same `subscriptionId` renews rather than duplicates.
  `detail.cancel` ends it immediately.
- `detail.ended` reasons: `expired` (collector lease lapsed), `cancelled`
  (collector-side cancellation), `withheld` (local sharing policy), `unsupported`
  (capability absent), `busy` (local resource budget), `failed` (sampling
  failed).
- A `detail.result` for an unknown or cancelled subscription is ignored by the
  hub. Results are matched by `subscriptionId`.
- Equivalent browser viewers are combined by the hub; the collector sees one
  lease per machine, not one per browser tab.

## 8. Implementation notes (Go)

Package `internal/protocol` owns:

- all constants in §1 and `Version`;
- every message, record, metric, inventory and enum type;
- `DecodeCollectorMessage([]byte)` and `DecodeHubMessage([]byte)`, strict
  decoders that enforce the byte limits and name the offending path, for example
  `samples.records[3].payload.cpu.usagePercent: 150 outside 0..100`;
- `EncodeCollectorMessage` / `EncodeHubMessage`, the validated outbound API.
  They marshal a value, re-run the strict decoder over the result (which also
  enforces the wire size limits) and reject typed-nil metric families or list
  members instead of emitting `null`. **Senders must use these, not
  `json.Marshal` directly.** Every type still implements `json.Marshaler` so the
  encoder can be reused, but the exported guarantee comes from the validated
  entry points.
- Conformance to the shared timestamp grammar and Unicode well-formedness rules
  described in §1.

Optional-nullable fields use `*Optional[T]`: a nil pointer is absent, a non-nil
`Optional` with `Valid == false` is an explicit `null`, and `Valid == true`
carries the value. Required-nullable fields (for example
`gpu.utilizationPercent`) use plain pointers where nil is `null`. Neither form
ever encodes an invented zero.

`testdata/protocol/` holds the golden fixtures:

- `valid/` and `invalid/` are named with a `collector-` or `hub-` direction
  prefix; invalid names state the defect.
- `reencoded/` is generated by `go generate ./internal/protocol` from the valid
  fixtures and is consumed by the TypeScript fixture test, proving Go-encoded
  output is accepted by the TypeScript parsers.

Both suites must pass:

```
go test ./...
npm run typecheck
node scripts/test.cjs
```

## 9. Deferred

- Multi-hub fan-out, per-destination sharing policy and disk budgets are not part
  of v1; one collector talks to one hub.
- Recordings and binary encodings are out of scope.
- Prometheus export and browser API contracts are separate documents.
