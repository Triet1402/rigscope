# Telemetry storage, history and loss

Rigscope defaults to **RAM telemetry history**. Incoming readings, summaries,
sample receipts, gaps and inventory stay in a private in-memory SQLite database,
with a one-second RAM ring for recent detail. A hub restart loses this history.

To save summary history to disk, run:

```sh
rigscope configure --telemetry-mode persistent
```

Rigscope shows the change, asks before stopping running services, saves the
setting and restarts them. Use `--system` to explicitly select a system install,
`--yes` for scripts, or `--no-restart` to leave services stopped after the change.
Switch back with `rigscope configure --telemetry-mode memory`.

Administrators can see the current mode, disk and RAM use and the oldest
retained history in Administration → Hub settings. Choosing the other mode
there shows what happens to existing history and gives the exact command to
run on the hub machine; the dashboard cannot restart the hub itself.

**RAM history is not copied when switching modes.** Restarting loses current RAM
history. Existing disk history is preserved in memory mode and becomes available
again in persistent mode. The setting is `components.hub.telemetryMode`;
omitted or `"memory"` selects RAM.

The collector always uses a bounded RAM queue (10 minutes or 64 MiB); it has no
persistent storage mode. Its identity and credentials remain saved on disk.

Configuration, accounts, sessions, collector identities, alert state and machine
names/tags remain durable in both modes. RAM mode eliminates routine telemetry
disk writes, but administrative changes and alert evaluation can still write.
There is no raw per-sample table or recording feature.

## What is stored where

The following table describes **persistent mode**. In RAM mode, summary history,
receipts, gaps, inventory and the latest snapshot are all volatile.

| Data | Storage | Survives a hub restart? |
| --- | --- | --- |
| 1-second history ring | Process memory | No |
| 10-second, 1-minute, 1-hour summaries | SQLite | Yes, within retention |
| Sample receipts (deduplication) | SQLite | Yes, 7 days by default |
| Gap records | SQLite | Yes, up to 400 days by default |
| Latest live snapshot | Memory, derived from the database on startup | Reconstructed |
| Accounts, password hashes, sessions | SQLite | Yes |
| Collector registry, grants, credentials | SQLite (credentials hashed) | Yes |
| Machine names, tags, inventory | SQLite | Yes |
| Alert rules, incidents, delivery jobs | SQLite | Yes |
| Process detail | Never persisted; request-scoped | No |

## Default retention

| Tier | Step | Retention |
| --- | --- | --- |
| Ring | 1 second | 15 minutes |
| Summary | 10 seconds | 2 hours |
| Summary | 1 minute | 30 days |
| Summary | 1 hour | 400 days |
| Sample receipts | — | 7 days |
| Gaps | — | 400 days |

The ring is bounded per hub as well as per machine: 64 MiB total and 64 machines
by default, with a 15-minute window. Each series keeps `min`, `max`, `sum`,
non-null `count` and an explicit unavailable `null` count per bucket. When the
ring budget is full, the oldest machine and series are evicted first; new
cardinality that still does not fit is rejected and counted, never silently
rolled into another bucket.

There is no raw tier in 1.0. The 1-second view comes from the ring when it covers
the requested range; otherwise the query uses the finest summary tier
that covers the range. An empty finer tier falls through to a coarser one
instead of hiding older data.

## Write path

- The collector sends batches of up to 32 records. The hub validates the batch
  strictly, then enqueues it into a bounded queue (4,096 records by default).
- A background writer batches persistent commits for up to **5 seconds**
  (RAM acceptance keeps its **1-second** delay), up to 512 records per
  transaction. Full batches and explicit flushes commit sooner. Live publication
  happens immediately and independently of that commit.
- Each transaction combines incoming 10-second buckets that share a minute or
  hour target, updating each summary row once per transaction.
- A sample is acknowledged only after acceptance commits to the selected store.
  Until then the collector retains it and retries. RAM mode advertises
  `delivery: "memory"`; an ack is RAM acceptance, not crash-safe persistence.
  Persistent mode advertises `delivery: "durable"` and uses WAL with
  `synchronous=FULL` before acknowledging.
- Records are deduplicated by `(collectorId, runId, sequence)` before any point
  or rollup side effect. Committed records cannot be counted twice.
- Retries are safe: a duplicate of a committed record is acknowledged as
  accepted. A record that can never succeed is rejected permanently and dropped,
  not retried forever.
- A graceful hub shutdown flushes pending summaries within its bounded shutdown
  window.

The collector's in-flight window and the hub's welcome window are both 64
batches, the protocol maximum. At the fastest supported 100 ms cadence that is
6.4 seconds of coverage, above the five-second persistent batch window.
Acknowledgments still arrive within the collector's 30-second batch timeout
under normal load. A graceful shutdown bypasses the batching delay.

## Replay

In RAM mode, deduplication receipts survive only for the current hub process.
A retry after a hub restart can be accepted again; RAM acknowledgments never
promise cross-restart deduplication or durable history.

Replay fills history; it never updates live state, appears fresh or opens an
alert. A replay batch is accepted only when its capture time is within the
**replay horizon**, 24 hours by default. A collector clock may run up to 24 hours
ahead of the hub. Records older than the horizon or further in the future are
rejected permanently, because a deduplication receipt may already have been
pruned.

Receipts are kept for 7 days, longer than the replay horizon plus clock skew.
This is what makes a retry safe even after chart retention has shortened. A
replayed sample extends the correct immutable summary bucket; it never
recomputes or inflates a completed total.

## Gaps and unknown loss

The collector keeps a bounded RAM queue: 10 minutes or 64 MiB, whichever comes
first, plus a hard ceiling on retained gap records. It does not write a durable
outbox in 1.0.

Known loss is reported honestly:

- When the queue drops records because of age, bytes or overflow, the collector
  emits a `gap` record covering the interval it observed, with a dropped-record
  count. A gap occupies a sequence number and is acknowledged like a sample.
- The hub also derives a `missing` gap when consecutive receipts are more than
  `max(15 seconds, 3 × advertised cadence)` apart.
- A permanent rejection is dropped and logged; it is not re-described as a
  fabricated gap.

Unknown loss is also reported, without invented numbers:

- A crash or forced stop discards the pending RAM queue. The collector cannot
  know the first lost capture time or how many records were lost.
- Every collector launch gets a new `runId`. When the hub stores records from a
  new run, the interval between the previous run's last record and the new run's
  first record becomes a `missing` gap with reason `collector restarted` and
  **no** `droppedRecords` count.
- Charts segment at that boundary, so a line or area never connects across a
  restart. Consumers must treat it as unknown loss, not a healthy zero.

A graceful shutdown drains for up to 3 seconds to reduce pending loss. A normal
service restart still creates a new run and a restart boundary in history.
That boundary means continuity is unknown; it does not prove samples were lost
or supply a dropped-record count.

## Retention and pressure

A retention pass runs about once a minute, deleting old rows in bounded chunks
and pruning orphaned series. It never starts from a request and never blocks
live publication behind a long cleanup. Each tier is deleted once it is older
than its own window; the one-hour tier therefore outlives the rest.

In RAM mode the summary database has a **128 MiB page budget**, with retention
pressure starting at 96 MiB, plus the separately bounded 64 MiB one-second ring.
These bound the retained database/ring data, not total process RSS: connections,
query work, queues and caches also use memory. If cleanup cannot free enough
space, new samples receive a busy response rather than unbounded growth.

In persistent mode the hub database has a physical budget of **2 GiB** by default, covering the
main file and its WAL, with a 128 MiB WAL reserve. Retention targets the budget,
not a fixed row count. When storage is full:

- Rigscope tries to make room by pruning the oldest history first.
- If it cannot, new samples are answered with a bounded busy response and the
  collector retries. Existing data is not discarded silently to make room.
- The hub enters a visible degraded state and reports storage pressure; it does
  not pretend writes succeeded.
- A disk-full failure is surfaced as a storage error. Live readings from the
  last accepted record may continue to be shown, clearly as last known data.

These are application limits, not filesystem quotas. Keep space for the
database, its WAL, upgrade backups (`backups/`) and the staged release.

## Query limits

History queries choose the finest tier that covers the requested range and fits
the scan budget. They return at most 600 points by default; a larger `maxPoints`
is clamped to 2,000 rather than rejected. At most 25,000 rows and 100 ms are
scanned per query, and at most 64 series are returned (up to 1,024 when per-core
CPU is requested). A query that exceeds a budget sets `truncated` and marks the
result partial. Empty buckets are omitted and unavailable readings have null
values.

History requests are rate-limited per session with a token bucket of 8 requests
and one refill per 250 ms, shared by that session's tabs. This prevents one
browser from saturating the hub's read path.

## Removing recordings

Recordings are **not part of 1.0**. The recording panel, recording routes, file
storage and per-recording download links from 0.2 are removed. There is no
`HUB_RECORDINGS_DIR`, recording size budget or interrupted-recording metadata.

For an incident snapshot, use the History CSV/JSON download described in
[Exports](exports.md). It exports the loaded buckets, including min/max/average
and gaps, but it is a query result rather than a new persisted capture.
