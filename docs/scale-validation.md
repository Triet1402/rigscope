# Sustained scale validation

Status: implementation and local sustained measurement, 2026-11-04.

This document records the isolated sustained workload added after the earlier
`ops measure` burst. It is deliberately **not** a capacity guarantee. The
numbers below describe one host, one payload shape, one database and the
concurrency listed for each case. They do not prove that a larger fleet,
different hardware, slower storage or a different browser workload will behave
the same way.

## What was missing

The earlier `ops measure` command was a bounded synthetic burst capped at 50
synthetic machines; it queues a fixed number of records and by default runs
History query iterations sequentially unless --history-concurrency is supplied. It never opens a service socket, so it
cannot show concurrent browser requests, SSE delivery, slow consumers,
collector reconnects or replay. Those paths are now measured by a standalone
harness instead of being inferred from the burst runner.

`src/operations/measure.ts` was also extended:

- the synthetic machine bound is now 1,000 instead of 50,
- `historyConcurrency` initiates overlapping History request rounds and reports
  their wall time, p95 and query count,
- the existing CLI arguments and report fields remain compatible.

## Harness and isolation

The harness is `scripts/scale-harness.ts`. It is a standalone source-checkout
tool, separate from the installed operational CLI. With development dependencies
installed, run it with `tsx`:

```sh
npx tsx scripts/scale-harness.ts --suite \
  --json-out docs/scale-validation.json
```

The parent process:

1. creates a fresh `0700` temporary directory and a `0600` hub configuration,
2. writes a throwaway SQLite database path,
3. spawns the same script in `hub` child mode on an ephemeral `127.0.0.1`
   port,
4. logs in real browser sessions through `/api/auth/login` (including the
   normal scrypt work and login token bucket),
5. connects real collector WebSockets through `/api/collector/connect`,
6. reads authenticated `/api/live` SSE streams and `/api/history` responses,
7. stops the child, closes the database and removes the temporary directory
   unless `--keep` is set.

The child is composed from the production modules: `HubState`,
`TelemetryStore`, `SampleIngestion`, `CollectorGateway`, `LiveFeed`,
`BrowserHttpAuth`, `BrowserSessionStore` and `MonitorHttp`. The database uses
WAL, `synchronous=FULL` and the real commit-before-ack path. The only synthetic
parts are the metric values and the cadence driver. No live hub, service
socket, production database or Tailscale endpoint is contacted.

The harness exposes a private `GET /__scale/stats` control endpoint on the
isolated child. It returns event-loop delay percentiles, event-loop
utilization, process CPU/RSS, database/WAL bytes, telemetry store counters,
connected collector count and SSE client count. The endpoint is not part of
the hub and is never reachable from a deployment.

## Workload

Each case uses representative metrics: eight CPU cores, two GPUs, two
filesystems, two disk devices, two network interfaces, plus the optional
bottleneck fields (temperature, package power, I/O wait, pressure, inode use,
disk latency and utilization). One record carries roughly 50 stored series
points before rollups.

| Case | Machines | Measured phase | Live cadence | History viewers | SSE viewers | Special workload |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| `small-5m` | 5 | 60 s | 1 s | 4 | 2 | small-fleet baseline |
| `fleet-75m` | 75 | 60 s | 1 s | 8 | 4 | beyond the old 50-machine cap |
| `fleet-75m-churn` | 75 | 60 s | 1 s | 8 | 4 | 15 collectors reconnect every 15 s with 20 replay records; SSE viewers reconnect with `Last-Event-ID` |
| `fleet-75m-slow-sse` | 75 | 60 s | 1 s | 8 | 6 | two SSE viewers pause reading for 15 s, measuring stale delivery and any buffer drop/reconnect |
| `fleet-75m-history` | 75 | 60 s | 1 s | 8 | 2 | broad History metric set every 300 ms per viewer |

Before each measured phase the harness connects all collectors and waits
1.5 seconds. The measured phase starts after that warmup. Records are counted
from collector startup, so the ingestion counters include the warmup and bounded drain tail; the
event-loop, CPU, RSS and database-growth windows are reset at the measured
phase boundary.

### Bounds

- machines: 1–1,000,
- measured phase: up to 1 hour,
- sample cadence: at least 100 ms (the protocol rate limit),
- records per live batch: 1–32,
- viewers: 1–50 authenticated sessions,
- SSE clients: 1–64,
- replay: at most 128 records per reconnect,
- estimated live plus replay records: at most 500,000 per case,
- database: 512 MiB main+WAL by default (`--database-budget-mb 0` disables the
  cap for inspection only),
- temporary database/config directory: removed after the case unless
  `--keep` is used.

## Host

The measurement host was:

- AMD EPYC 7402P 24-Core Processor, 48 logical CPUs,
- 121 GiB usable memory (130,351,214,592 bytes reported by the OS),
- Linux 7.3.0-9-generic, Ubuntu 26.10, x86_64,
- Node.js 26.13.0,
- local NVMe root filesystem (the harness database lived under `/tmp`),
- load average 0.92 / 0.76 / 0.84 around the run; unrelated user services were
  active, so this is not a quiet benchmark host.

`monitorEventLoopDelay` used a 10 ms resolution. The reported p50 therefore
floors near 10 ms even when the process is mostly idle; p95/p99/max are the
meaningful tail values.

## Results

All five cases completed with zero rejected records, zero `retry` acknowledgments,
zero unacknowledged live records, zero write failures and no disk-budget
pressure. The event-loop window excludes the 1.5 s collector warmup. "History /
SSE" is authenticated History sessions / SSE clients. "Sample→commit" in the original report is capture-to-durable-ack latency: an
upper bound on commit lag that also includes the acknowledgment return path.
It is measured on the same host clock, not across uncalibrated remote clocks.
"Normal SSE lag" is the
age of the newest captured sample when an `update`/`snapshot` arrives at a
normal viewer; it includes the hub's 500 ms coalescing window. Hub CPU is the
hub child's process CPU as a percentage of one core, not the whole host.

| Case | Machines | History / SSE | Records accepted | Ack p95 | Sample→commit p95 | History p95 / max | Normal SSE lag p95 | Event-loop p99 / max | Hub CPU | RSS peak | DB+WAL growth |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `small-5m` | 5 | 4 / 2 | 317 / 317 | 26.7 ms | 27 ms | 12.9 / 20.0 ms | 526 ms | 12.5 / 18.5 ms | 2.6% | 135.9 MiB | 4.63 MiB |
| `fleet-75m` | 75 | 8 / 4 | 4,789 / 4,789 | 55.6 ms | 56 ms | 20.9 / 35.2 ms | 439 ms | 30.4 / 44.6 ms | 12.4% | 188.7 MiB | 12.08 MiB |
| `fleet-75m-churn` | 75 | 8 / 4 | 4,845 / 4,845 | 52.2 ms | 52 ms | 23.4 / 35.0 ms | 438 ms | 29.0 / 42.4 ms | 14.2% | 203.2 MiB | 14.22 MiB |
| `fleet-75m-slow-sse` | 75 | 8 / 6 | 4,791 / 4,791 | 64.4 ms | 64 ms | 23.0 / 36.6 ms | 502 ms | 38.5 / 76.2 ms | 12.0% | 190.2 MiB | 12.95 MiB |
| `fleet-75m-history` | 75 | 8 / 2 | 4,785 / 4,785 | 70.7 ms | 71 ms | 50.3 / 60.5 ms | 453 ms | 48.0 / 72.7 ms | 31.2% | 229.7 MiB | 12.98 MiB |

Additional observations:

- **History authorization budget held.** The broad case ran 1,608
  authenticated queries (8 sessions polling every 300 ms) with 1,608 `200`
  responses, zero `429` responses and zero errors. Each response returned
  about 25 series and 800 points; the 25,000-row/100 ms budget was not
  truncated.
- **Collector reconnect/replay recovered.** The churn case performed 45
  collector reconnects and replayed 900 historical records; all 900 were
  accepted. First live acknowledgment after reconnect was 167.9 ms p95
  (175.8 ms max). The durable queue finished empty.
- **SSE reconnect/reset recovered.** The churn case reconnected 48 SSE streams
  and received 48 `reset` snapshots; all resets followed a reconnect. First
  event after reconnect was 10.1 ms p95.
- **Slow consumers stayed connected but stale.** The two paused viewers
  received no new bytes for 15 s and then read buffered snapshots. Their
  delivery lag was 11.6 s p95 while normal viewers stayed at 502 ms p95. On
  this loopback path no slow-viewer drop or reconnect was observed, so the
  hub's 1 MiB writable-buffer guard did not fire; kernel socket buffering
  likely absorbed the pause. The deliberate disconnect/reconnect path is
  covered by the churn case instead.
- **Database/WAL stayed bounded.** The final main+WAL size was 14.8 MiB for
  the 75-machine baseline, 16.9 MiB with churn, and 15.7 MiB with broad
  History. The 512 MiB measurement budget was never approached, no checkpoint
  pressure appeared, and the store queue was empty at the end of every case.
- **Small fleet stayed cheap.** Five machines, four History viewers and two SSE
  viewers used 2.6% of one core and stayed at 12.5 ms event-loop p99.

The full machine-readable report, including per-case store counters, database
sizes, SSE byte counts, tier counts and host load averages, is
[scale-validation.json](scale-validation.json). The generated JSON is the raw
artifact; this page is the summary.

## Interpretation

These results support the following limited statements on this host:

- A 75-machine, 1-second-cadence synthetic fleet can be ingested with
  commit-before-ack p95 under 100 ms while eight authenticated History viewers
  and four full-fleet SSE viewers run concurrently.
- Collector reconnect plus 20-record replay per affected machine recovered
  within a few hundred milliseconds in this run, without rejected or unacked
  records.
- SSE reconnect with `Last-Event-ID` produced a fresh `reset` snapshot within
  about 10 ms on loopback, and the next update was fresh again.
- Broad History polling near the per-session token budget raised the
  event-loop tail to about 48 ms p99 and used about 31% of one core, but
  produced no 429s and no truncated responses at this data size.

They do **not** support claims that:

- 75 machines is a capacity limit or a safe upper bound,
- a larger fleet, more viewers, slower storage or a different payload will meet
  the same latencies,
- the hub is multi-core, clustered or horizontally scalable,
- slow consumers are always disconnected before they fall behind,
- the default 100 concurrent collector socket budget should be raised without
  a deployment-specific measurement.

The 75-machine cases used the default 100-connection budget. A deployment that
raises `HUB_MAX_COLLECTOR_CONNECTIONS` must re-run the harness with its own
payloads, storage and viewer counts.

## Reproduction

```sh
# full five-case suite (about five minutes)
npx tsx scripts/scale-harness.ts --suite \
  --json-out docs/scale-validation.json

# one case with overrides
npx tsx scripts/scale-harness.ts --case fleet-75m \
  --machines 75 --viewers 8 --sse-clients 4 --duration-ms 60000 \
  --json-out /tmp/rigscope-scale-75m.json

# retain the throwaway database and configuration for inspection
npx tsx scripts/scale-harness.ts --case small-5m --keep
```

The short `tests/scale-harness.test.ts` smoke runs the same child with two
machines, collector reconnect/replay and an SSE reset. It is not a capacity
test; it proves the harness paths work in CI.

## Limits

- The hub child and all load generators share one host. The hub event loop uses
  one thread; the load processes can run on other cores. This is
  not a distributed-load test and the generator competes with the hub.
- History queries are synchronous SQLite work on the hub event loop.
  "Concurrent viewers" means overlapping authenticated HTTP requests, not
  parallel SQLite readers.
- The payload is representative but not captured from a real fleet. Different
  process counts, GPUs, filesystems or optional fields change the stored row
  count.
- The host may run unrelated services. Load averages and CPU are recorded in
  the JSON artifact, but they are not a controlled benchmark environment.
- Passing these cases does not remove the concurrent collector socket budget
  (default 100, configurable with `HUB_MAX_COLLECTOR_CONNECTIONS`) or the
  history row/elapsed/frame/storage budgets.
- Authentication and machine scopes use the production modules with loopback
  origin rules. This harness does not exercise the deployed HTTPS/private-socket
  boundary; its separate runtime and actual-path checks remain distinct.
- A result is evidence for this host and workload only. It is not a claim of
  unlimited machine, viewer or History capacity.


## Root integration acceptance

After integration, a separate 75-machine, eight-second churn run used eight
History sessions and four SSE consumers. Its authorization canary required
anonymous SSE 401, an allowed scoped History query 200, a forbidden machine query
403, and correctly filtered initial and reconnect/reset SSE snapshots. All passed.
The run accepted 930 live records plus 600 replay records, recorded 30 collector
reconnects, finished with zero unacked live records and zero write failures.
`scale-acceptance.json` records this shorter integration check. It is not another
60-second capacity measurement; the five sustained results above remain the
worker's original raw artifact from before the authorization canary was added.

The original `sampleToCommitLagMs` field names are retained for compatibility.
Those numbers are capture-to-durable-ack upper bounds on commit latency, including
the return path; they are not a server-side SQL commit timestamp. Queue/backpressure
counters and end-of-run queue emptiness are recorded, but the original report does
not sample an exact queue high-water mark. Slow viewers did not reach the server's
buffer-disconnect threshold in these runs, so that threshold is not a measured
fleet-capacity limit.
