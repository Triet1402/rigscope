# Hub capacity stress test — 2026-11-04

On this AMD EPYC 7402P host (24 cores / 48 threads, 121 GiB RAM), the isolated
hub passed **500 collectors plus 64 concurrent live browser sessions** for two
minutes. Every collector sent one representative sample per second; every
session also polled History once per second. Ack p95 was **223 ms**, History p95
was **181 ms**, hub CPU averaged **75% of one core**, and sampled hub RSS peaked
at **560 MiB**. All 64 streams stayed connected. All 7,744 History requests
succeeded, all submitted live records were acknowledged, and the final queue
was empty.

These are synthetic measurements on this machine, using production hub modules.
They are **not a long-term capacity guarantee or a test of the deployed HTTPS
service, real browser rendering, or a mature multi-day history database**.

## Three different limits

- **Application limits:** 100 collector sockets by default, configurable with
  `HUB_MAX_COLLECTOR_CONNECTIONS`; 50 user accounts; 64 simultaneous live browser
  streams; 256 stored browser sessions. Accounts, logged-in sessions, and open
  streams are different counts. The tests used independent sessions on one
  shared all-machines account, plus a scoped account for authorization checks.
- **Ingestion without live streams:** 1,050 collectors passed the two-minute
  target; 1,125 exceeded the chosen 500 ms ack p95 target (538 ms). Thus the
  observed two-minute latency boundary is **between 1,050 and 1,125** at 1 Hz.
  At 1,200, ack p95 rose to 2.15 seconds, retries appeared, and the final queue
  held 2,321 records. This is an ingestion measurement with one light History
  session, not a usable full-fleet dashboard capacity.
- **Full-fleet snapshot delivery:** 529 collectors passed, but 530 disconnected
  even one fast loopback viewer after its initial snapshot. The 529-machine
  snapshot was 1,047,111 bytes; at 530 it was 1,049,092 bytes. The live feed calls
  `res.write()` and then destroys the connection when `writableLength` exceeds
  its **1,048,576-byte guard**. A single sufficiently large snapshot triggers
  that guard. The 530-machine run kept ingestion healthy (~532 commits/s,
  58 ms ack p95) but delivered **zero live updates**, confirming that the failure
  is in streaming. The exact machine count depends on payload size and access
  scope; the 1 MiB guard is the relevant constraint.

A separate admission probe opened 65 streams: **64 returned HTTP 200, and one
returned HTTP 503**. That is the configured stream cap, not exhausted hardware.

Source references: [collector socket budget](https://github.com/Triet1402/rigscope/blob/main/src/hub/collector-ws.ts),
[collector configuration](https://github.com/Triet1402/rigscope/blob/main/src/hub/config.ts),
[user account limit](https://github.com/Triet1402/rigscope/blob/main/src/hub/auth/config.ts),
[session capacity](https://github.com/Triet1402/rigscope/blob/main/src/hub/auth/sessions.ts), and
[live feed admission and buffer guard](https://github.com/Triet1402/rigscope/blob/main/src/hub/sse.ts).

## Measurements

| Collectors | History / live sessions | Seconds | Ack p95 | History p95 | Hub CPU¹ | Hub RSS peak | Result | Evidence |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |
| 900 | 1 / 0 | 120 | 72 ms | 27 ms | 75.1% | 225 MiB | Pass | [182625-probe-m900-u1-s0-120000ms.json](182625-probe-m900-u1-s0-120000ms.json) |
| 1,050 | 1 / 0 | 120 | 140 ms | 28 ms | 82.2% | 260 MiB | Pass | [182847-probe-m1050-u1-s0-120000ms.json](182847-probe-m1050-u1-s0-120000ms.json) |
| 1,125 | 1 / 0 | 120 | 538 ms | 31 ms | 86.7% | 259 MiB | Outside target | [183112-probe-m1125-u1-s0-120000ms.json](183112-probe-m1125-u1-s0-120000ms.json) |
| 1,200 | 1 / 0 | 120 | 2,154 ms | 30 ms | 92.4% | 262 MiB | Outside target | [182354-probe-m1200-u1-s0-120000ms.json](182354-probe-m1200-u1-s0-120000ms.json) |
| 1,800 | 1 / 0 | 20 | 88 ms | 24 ms | 103.4% | 291 MiB | Pass | [181114-probe-m1800-u1-s0-20000ms.json](181114-probe-m1800-u1-s0-20000ms.json) |
| 1,800 | 1 / 0 | 120 | 3,872 ms | 34 ms | 108.3% | 311 MiB | Outside target | [182108-probe-m1800-u1-s0-120000ms.json](182108-probe-m1800-u1-s0-120000ms.json) |
| 500 | 64 / 64 | 120 | 223 ms | 181 ms | 74.7% | 560 MiB | Pass | [193608-probe-m500-u64-s64-120000ms.json](193608-probe-m500-u64-s64-120000ms.json) |
| 529 | 1 / 1 | 15 | 58 ms | 9 ms | 45.7% | 200 MiB | Pass | [193909-probe-m529-u1-s1-15000ms.json](193909-probe-m529-u1-s1-15000ms.json) |
| 530 | 1 / 1 | 15 | 58 ms | 18 ms | 45.0% | 189 MiB | Outside target | [193940-probe-m530-u1-s1-15000ms.json](193940-probe-m530-u1-s1-15000ms.json) |
| 100 | 1 / 65 | 10 | 64 ms | 5 ms | 15.7% | 162 MiB | Outside target | [194011-probe-m100-u1-s65-10000ms.json](194011-probe-m100-u1-s65-10000ms.json) |

¹ Hub CPU is process CPU as a percentage of one core, including its helper
threads. It is not the percentage of the 48-thread machine. Windows and
percentiles have the boundaries described below. “Outside target” includes
intentionally exceeding admission limits; it does not mean a hub crash.

The 1,800-collector result is included to show why a short burst is insufficient:
it passed for 20 seconds, then failed over 120 seconds with 3.87-second ack p95,
retry acknowledgments and a growing queue. The 1,125 run crossed only the ack
latency target; it did not lose submitted records or finish with backlog.

## Method and pass criteria

The existing `scripts/scale-harness.ts` creates a fresh private temporary
configuration and SQLite database for every run and binds a throwaway child to
an ephemeral loopback port. It drives real authenticated collector WebSockets,
SSE and History HTTP requests through production modules, including durable
commit-before-ack, WAL and `synchronous=FULL`. The installed service and its
configuration/database are never used as load targets.

The payload represents eight CPU cores, two GPUs, two filesystems, two disk
devices and two network interfaces, with optional bottleneck counters. Each
collector sends one record every 1,000 ms. History requests select one machine,
four standard series and a five-minute range, though the fresh database only
contains the run's data. All live sessions receive the full fleet. Enrollment,
process detail, active alert rules/webhooks, static page delivery and TLS are
outside this workload.

The isolated collector admission budget is raised automatically above the
requested fleet size. This does not change a deployed hub's 100-connection
default. The queue was aligned with production at 4,096 records and transactions
at 32 records. The session store uses the production 256-session default. The
live-stream cap remains 64. Database main+WAL was capped at 1 GiB for these
experiments; no run reached the cap or reported SQLite write failures.

An initial exponential search doubled load, then bisected the passing/failing
interval to about 12.5%. Follow-up two-minute runs narrowed the ingestion
boundary, and single-machine probes narrowed the snapshot boundary. Targets
were chosen for this experiment, not taken from a published product SLA:

- ack p95 ≤500 ms; History p95 ≤1,000 ms;
- hub event-loop p99 ≤200 ms;
- normal SSE newest-sample age p95 ≤2,000 ms;
- measured durable commit rate ≥95% of requested cadence;
- successful History polls ≥90% of requested cadence;
- no retries, rejected/unacked records, collector send errors, or cadence skips;
- no History errors, HTTP 429s or truncated queries;
- all requested live streams remain present and receive ongoing updates;
- final queue empty, with no write failures or physical-budget pressure.

Durable throughput is computed from the difference in the hub's commit counters
across the measured window. Ack latency is send-to-durable-ack round-trip time,
including startup and the drain tail. SSE lag is the age of the newest machine
sample in each received snapshot; it does not establish freshness of every
machine. History/SSE totals and percentile samples include the bounded tail.

### Correcting the load generator

The first mixed-load search used a single parent event loop for collectors,
History polling and parsing all 64 streams. It appeared to stop around 400–450
collectors, but it was under-producing samples while every submitted record
was still accepted. Those cases remain in the raw index as diagnostic evidence;
they are **not** the mixed-load capacity conclusion.

The harness now supports `--sse-workers N`. The final mixed runs use four
independent stream-reader processes, with full JSON parsing, and a one-second
client warmup before resetting the measured window. The collector driver and
History polling stay in the parent process. The final 500/64 run achieved
99.96% of requested durable sampling throughput. Single-stream boundary probes
use one reader worker. Older results without workers include stream startup
in the measured window. A three-second, two-machine worker smoke is also in
the index; its cadence classification is not a capacity result.

### Resource protection

`scripts/stress-hub.py` supervises the entire test process group at one-second
intervals, including the hub, driver and stream readers, at reduced scheduling
priority. It stops the group for:

- less than 16 GiB available host RAM;
- more than 4 GiB summed test-process RSS;
- less than 10 GiB free disk;
- more than 256 MiB of new host swap-out;
- over 80% host CPU for five consecutive samples;
- excessive setup/measurement time.

Across the 29 measured attempts, sampled test-process RSS peaked at **1,475 MiB**,
host CPU peaked at **9.69%**, available RAM stayed above **108.2 GiB**, and new
swap-out was **zero**. No resource cutoff fired. Existing used swap was about
7.6 GiB before testing; it was not new pressure from these tests.

The hub's CPU and synchronous storage work become limiting while most host
cores and RAM remain available. The final mixed run received about 11.7 GB of
SSE data including warmup/drain; loopback does not establish capacity over a
real network link. Different CPU-core counts, devices, scopes, History breadth,
retention age or network conditions can change these results substantially.

## Reproduce

Requires Linux, Python 3, Node 26+ and the checkout's development dependencies.
Run from the repository root. Each attempt saves raw JSON, a log and a resource
trace under the dated output directory; `--out PATH` overrides it. Logs and
per-process resource traces are ignored by Git and stay local. Measurement JSON
can be committed with a reviewed report.

```sh
# Quick exponential/binary ingestion search; follow with longer confirmation
python3 scripts/stress-hub.py --search collectors --machines 100 --sse 0   --duration-ms 20000

# Passing two-minute ingestion load and its nearby failing target
python3 scripts/stress-hub.py --machines 1050 --users 1 --sse 0 --duration-ms 120000
python3 scripts/stress-hub.py --machines 1125 --users 1 --sse 0 --duration-ms 120000

# Confirmed two-minute mixed workload; stream workers default to four
python3 scripts/stress-hub.py --machines 500 --users 64 --sse 64 --duration-ms 120000

# Snapshot-size and stream-admission probes (expected failures at 530 and 65)
python3 scripts/stress-hub.py --machines 529 --users 1 --sse 1 --sse-workers 1 --duration-ms 15000
python3 scripts/stress-hub.py --machines 530 --users 1 --sse 1 --sse-workers 1 --duration-ms 15000
python3 scripts/stress-hub.py --machines 100 --users 1 --sse 65 --duration-ms 10000
```

The runner records a failed target as `"pass": false` in its report and continues
a search; intentional over-limit probes are ordinary successful runner
executions. Read the report rather than using the Python exit status as a
performance assertion.

## Artifacts and verification

[All 29 attempts](index.jsonl), [chart](results.html), and
[source hashes and cleanup manifest](manifest.json). Each index entry identifies
its raw report and resource trace; a sibling `.log` holds runner output. Raw
measurement reports are versioned, including failed and superseded generator
configurations. Detailed resource traces and logs are retained locally and
excluded from Git; regenerate them with the commands above when needed.

Verification: `npm run typecheck`, `git diff --check`, and both
`node --import tsx --test tests/scale-harness.test.ts` cases passed. The tests
exercise authenticated scope checks, collector reconnect/replay, SSE reset and
cleanup with zero and two independent reader workers. Python syntax was checked.
All recorded temporary hub/database directories were removed, and the installed
hub returned HTTP 200 on its existing Unix-socket health endpoint afterward.
The installed hub and collector processes were left running. Only harness,
supervisor, tests and documentation files changed.
