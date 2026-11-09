# Telemetry storage, history and recording

Rigscope has two explicit telemetry-history modes. The default is RAM-only.
Persistent history is opt-in and writes compact summaries, not one row per
collector reading.

> "No persistent telemetry history" is accurate for memory mode. It is not a
> promise of zero disk writes: hub logs, SQLite control state, alert state,
> recordings and operating-system swap may still write.

Configuration and non-history state always remain in SQLite: users, password
hashes, sessions' audit records, collector enrollment and credentials, grants,
alert rules/incidents/jobs/webhook settings, machine display names/tags, and
inventory. Only telemetry history is ephemeral by default.

## Modes

| | `HUB_TELEMETRY_MODE=memory` (default) | `HUB_TELEMETRY_MODE=persistent` |
| --- | --- | --- |
| Live readings | Updated immediately in RAM | Updated immediately in RAM |
| Recent charts | Bounded typed-array RAM rings | Compact worst-case summaries in SQLite |
| Ack meaning | Accepted into hub RAM; intentional loss on restart | Committed transaction; durable delivery |
| Restart | History and latest snapshots disappear | Summaries and latest snapshots restore |
| Routine telemetry writes | None | One transaction roughly every 5 seconds |
| Deduplication | Per-run in-RAM interval set | `sample_receipts` with an independent 7-day TTL |

The hub sends `delivery: "memory"` or `"durable"` in `welcome`: it describes the
hub's choice, not the collector's. Collectors retry and hold in-flight batches
the same way in both cases, so delayed acknowledgements do not move sustained
writes from the hub to the collectors.

## Collector delivery queue

Unacknowledged samples wait in a bounded queue per destination. Its retention is
independent of the hub's history mode.

| | `COLLECTOR_QUEUE_MODE=memory` (default) | `COLLECTOR_QUEUE_MODE=durable` |
| --- | --- | --- |
| Pending samples | RAM | SQLite file per destination |
| Routine collector disk writes | None | One insert, one in-flight update and one delete per sample |
| Collector restart | Pending samples are lost | Everything unacknowledged is replayed |
| Reported to the hub | `delivery.queueMode: "memory"` | `delivery.queueMode: "durable"` |

Both modes share the same bounds (10 minutes and 64 MiB per destination, 256 MiB
aggregate), the same live-slot priority, and the same coalesced `gap` records
that tell the hub "samples were captured here and dropped" rather than "no
samples were captured". Choose `durable` when a collector restart losing a few
minutes of samples matters more than a database write per sample; `rigscope`
reports `queueMode` under a connection's delivery facts so the difference is
visible without guessing the configuration.

### Memory history defaults

The RAM budget is `HUB_HISTORY_MEMORY_BYTES` (default **128 MiB**) for the whole
hub, not per machine. Within it, each series keeps:

| Tier | Default retention | Purpose |
| --- | --- | --- |
| 1 second | 2 minutes | Short incident lead-up |
| 10 seconds | 1 hour | Recent troubleshooting |
| 1 minute | 24 hours | Medium-term history |
| 1 hour | 7 days | Longer-term trends |

Per-core CPU uses shorter detail: 10-second buckets for 10 minutes, 1-minute for
6 hours and 1-hour for 2 days. Other metrics use the table above.

Each reading updates the current bucket of each applicable tier. Buckets keep
`min`, `max`, `sum`, non-null `count` and an explicit unavailable `nullCount`, so
a one-second spike is still represented after it is aggregated into a minute or
hour. This is aggregation, not "keep every tenth sample". Empty buckets are
omitted; an all-unavailable bucket renders as unknown, never zero.

All current metrics are gauges or already-derived rates: the collector computes
network/disk bytes per second from monotonic counters before sending, so the hub
never averages a cumulative counter and does not need first/last values for rate
reconstruction. If a cumulative counter metric is added later, it needs an
explicit delta/first-last aggregation policy rather than this gauge path.

When a new series would exceed the global budget, the least-recently-seen series
lose their finest tiers first, then whole series are evicted. New cardinality
that still does not fit is rejected and reported as pressure. This makes the
budget finite even for a machine that adds hundreds of disks or cores.

### Persistent summary history

Persistent mode buffers ingestion and commits all series for roughly five
seconds in one transaction. It writes `points_10s` plus folded one-minute and
one-hour rollups. It does **not** write `points_raw` unless an operator
explicitly constructs the store in raw mode (used by tests and measurement).

- Live publication is decoupled from the commit, so the dashboard is not one
  flush behind.
- A sample is acknowledged only after `COMMIT` returns with `synchronous=FULL`.
- Retries are deduplicated by `(collectorId, runId, sequence)` before any
  point/rollup side effect. Committed records cannot be counted twice.
- After a crash, uncommitted buffered samples are replayed by the collector
  because they were never acknowledged. Committed samples have receipts.
- A graceful shutdown flushes pending summaries before closing within the hub's
  bounded shutdown window.
- Late/replayed samples extend the correct immutable summary bucket; the
  one-minute and one-hour rows receive only the new delta, never a recomputed
  total. Out-of-order samples inside the retained RAM ring update the matching
  completed bucket; samples older than the retained window are counted as late
  and dropped rather than corrupting a newer bucket.
- Buckets use absolute minute/hour boundaries. A query includes the current
  partial window once, and a later query recomputes only that bucket from the
  same summaries, so completed data is not double-counted.
- Flush failures resolve pending batches as retry/busy, so collectors keep the
  data and back off. The queue is bounded (`HUB` storage queue and the
  collector's in-flight window) and does not grow without limit.
- The collector's local in-flight window and the hub's welcome window are both
  64 batches, the wire maximum. The hub's persistent flush interval is capped
  at five seconds, which fits that window at the fastest supported 100 ms
  cadence (6.4 s of coverage) and leaves 1.4 s for COMMIT/ack latency; it is
  also far below the collector's 30-second per-batch ack timeout. The
  collector reserves enough slots for a full flush window of live samples,
  plus two samples of headroom, and drains a waiting live
  record as soon as an acknowledgement frees capacity. Its delivery queue keeps
  the existing 10-minute/64 MiB per-destination bounds in either mode, so
  delayed acks do not turn into unbounded queueing or silent data movement.

Dedup receipts default to **7 days** and are independent of chart retention.
Shortening raw/10-second history can no longer let a replayed sample recreate
expired chart data.

### Which readers exist

Charts, exports and recordings all use the same bounded history query interface,
so memory and persistent modes do not have separate chart code. Query work is
bounded by series count, scanned rows, elapsed time and output points. The
finest tier whose retention covers the requested range is selected; an empty
finer tier falls through to the next one (for example after upgrading an
existing database).

## Recording

A recording is an explicit, bounded capture and is separate from chart history.

- Start/stop from **History → Recording**.
- Choose the current machine, selected metrics, resolution (1s, 10s, 1m, 5m or
  1h), duration, and whether to include recent RAM history (the lead-up).
- The hub writes private JSON-lines data under `HUB_RECORDINGS_DIR`
  (default `<hub database directory>/recordings`, mode 0700 files 0600).
- Recordings are bounded by `HUB_RECORDING_MAX_DURATION_MS` (default 1 hour) and
  `HUB_RECORDING_MAX_BYTES` (default 64 MiB). A duration or size limit ends the
  capture with an explicit reason.
- Metadata survives restart. A capture active during a restart is marked
  interrupted and its partial file stays downloadable.
- Download/list/delete are authenticated and authorized to the creating account
  (administrators can see all) and to the caller's current machine scope. The
  browser names an opaque recording id; it can never name a filesystem path.

Recordings are not part of a database-only backup. If they matter, include the
recordings directory in the same private backup set as the hub database.

## The one-time machine-data transition

The first `rigscope upgrade` (or `rigscope system upgrade --apply`) after the
RAM-first release completes a one-time transition. It discards the previous
design's on-disk machine-derived data once, explicitly authorized for this
transition even for data an operator had opted into keeping:

- hub database: telemetry rows and their summaries, `sample_receipts`, `gaps`,
  `machine_latest`, `series`, `inventory`, and the machine-derived alert
  history (`alert_incidents`, `alert_machine_progress`,
  `alert_observed_resources`);
- the collector's pre-RAM-first durable outbox tree.

It always preserves accounts and sessions, collector enrollment and grants,
settings, alert rules/jobs/webhook configuration, machine display names and
tags (`machine_meta`), and recordings.

The transition is recorded in a version-independent
`machine-data-transition.json` marker in the installation's state root, written
only after the activated release verified healthy. Every upgrade after that —
including upgrades to future versions — reads the marker and preserves durable
history and queues you opt back into, so `COLLECTOR_QUEUE_MODE=durable` and
durable hub history survive later upgrades intact. An installation created by
`install` (fresh or reset) is recorded as already transitioned.

Until the marker is written the transition is reversible: the queue tree is
renamed aside rather than deleted, and the upgrade's recorded pre-upgrade
database copy restores the cleared rows if activation fails — even when the
previous runtime could still read the schema, because the transition removed
rows, not schema. A cleanup that cannot finish never fails an otherwise healthy
upgrade: it is reported as a visible warning, the marker stays unwritten, and
the next upgrade retries it. The upgrade output states exactly how much was
discarded.

If you skipped the transition window (for example after re-registering an
installation with `rigscope install`) and want old rows gone, use the explicit
reset below.

## Reset old history

Switching an existing installation to memory mode does not read or rewrite old
telemetry rows, but they still occupy the database if the one-time transition
above has not run. To discard them explicitly while preserving users,
enrollment, settings and alert state:

```sh
rigscope stop
node dist/ops.cjs reset-telemetry \
  --database /path/to/telemetry.sqlite \
  --backup-dir /path/to/private-backups \
  --confirm-stopped
```

The command deletes only `points_raw`, `points_10s`, `points_1m`, `points_1h`,
`sample_receipts`, `gaps`, `machine_latest` and `series`. It refuses without
`--confirm-stopped` and while the hub lock exists. It never drops the database
file, users, collector registry, alert rules/incidents, metadata, inventory or
recordings. `--backup-dir` is optional but recommended; a consistent backup is
taken first when supplied.

## Benchmark

`npm run benchmark:telemetry -- --machines 2 --seconds 120` drives the real
stores with a realistic ~186-series-per-machine, one-second payload. It reports
process-attributed `/proc/self/io` write bytes (not database size), ingestion
enqueue/commit latency, history-query latency, RSS, row counts and database
size. `--json` prints machine-readable output; `--dir` selects a real filesystem
because tmpfs does not account storage writes in `/proc/self/io`.

One development-host result for **2 machines × 60 simulated seconds** (186
series each) was:

| Mode | Steady writes | Close/checkpoint | DB size | Commit p95 | History query |
| --- | --- | --- | --- | --- | --- |
| memory | **0.00 MiB** | 248 KiB | 0.3 MiB | 42 ms | 3.0 ms |
| persistent summaries | **0.98 MiB** | 264 KiB | 1.2 MiB | 35 ms | 5.9 ms |
| legacy raw, per-batch commit | 21.66 MiB | 692 KiB | 21.6 MiB | 15 ms | 6.8 ms |

That run measured persistent summaries at about **0.05×** the legacy write
volume (roughly 22× less) and memory mode at zero sustained ingestion writes.

A second run with **10 machines** (1,860 series) and the default 128 MiB RAM
budget reported `RAM 128/128 MiB pressure` while keeping all series readable
through the coarser tiers: memory wrote 0.00 MiB, persistent summaries 4.05 MiB,
and legacy raw 104.76 MiB of steady ingestion-phase writes. RSS peaks include
the Node baseline and temporary query/flush allocations, so they are higher than
the history-array budget.

These numbers are development-host measurements, not a promised multiplier or
capacity guarantee. Re-run on the deployment host and payload before choosing
retention or budgets.

If an earlier transition attempt was interrupted, its staged collector queue is
kept for manual recovery; a retry does not delete another attempt's copy. If a
failed replacement runtime creates a new queue, rollback preserves that queue
separately and restores the original pending queue. An existing malformed or
unreadable transition marker blocks the upgrade before clearing data; repair
the marker rather than deleting it, which would request another reset.

Failure to write the completion marker is reported without failing a healthy
upgrade and leaves the transition pending. Failure to remove a staged queue
after the marker was written is also reported, but later upgrades still preserve
new history because completion has already been recorded.
