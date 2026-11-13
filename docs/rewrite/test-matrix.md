# Runtime test matrix

Status: 1.0. The runtime suites exercise the single static Go binary
(`cmd/rigscope`); there is no Node hub or Node collector to select, and no
fallback. The Go collector samples, enrolls, delivers, serves process detail
and drains on SIGTERM. The Go hub runs as one binary: one installation lock,
one SQLite handle shared by auth/admin/registry/telemetry/live/history, one hub
run id and one HTTP server, with browser sessions stored durably by token hash.

## Required environment

| Variable | Value | Meaning |
| --- | --- | --- |
| `RIGSCOPE_TEST_GO_BINARY` | absolute path | The `rigscope` binary built from this tree. Required; the suites fail immediately when it is missing, relative, not a file or not executable. |
| `RIGSCOPE_RUNTIME_TEST_CONCURRENCY` | `1`..`16` | Concurrent test files. Default `4`. |

`scripts/test-runtime.cjs` validates both before spawning test files. A missing
or unusable binary exits immediately with an actionable message; it never waits
for a boot timeout. Tests that need a collector also stop waiting the moment the
child exits and print its captured output, so an early exit is reported as the
collector's own failure instead of a 30 s SSE timeout.

The launches are:

```text
<binary> collector        --config <stateDir>/config.json --state-dir <stateDir> [--hostname NAME]
<binary> collector enroll --config <stateDir>/config.json --state-dir <stateDir> --grant TOKEN
<binary> hub              --config <stateDir>/config.json --state-dir <stateDir>
```

## Collector state tree

`tests/runtime/support/launcher.ts` writes a version-1 state tree (all private
files `0600`, the state directory `0700`):

```text
<stateDir>/config.json
  { "version": 1,
    "components": { "collector": {
      "enabled": true,
      "destinations": [{ "url": "ws://...", "caFile": "/pinned/ca.pem" }],
      "samplingInterval": "200ms",
      "synthetic": true,                 // mock/synthetic mode only
      "sharing": {
        "cpu": true, "memory": false, "swap": false, "gpus": true,
        "disks": true, "network": true, "networkAddresses": false,
        "processes": true, "processArguments": false
      },
      "hardware": { "gpuRegisters": false }
    } } }

<stateDir>/collector/identity.json
  { "collectorId": "..." }

<stateDir>/collector/credentials.json
  { "credentials": [{ "url": "ws://...", "token": "..." }] }
```

Translation notes:

- **One destination.** `destinations` is a list because 1.0 may grow back to
  multi-hub, but exactly one entry is accepted. The launcher rejects a
  multi-destination spec with a clear message. The bearer token never appears
  in `config.json`; it lives in `collector/credentials.json`.
- **Cadence.** The 0.2 `intervalMs` becomes `samplingInterval` (a Go duration
  string such as `"200ms"` or `"2s"`). The suite keeps one cadence per
  installation.
- **Sharing.** `components.collector.sharing` is the collector-local policy.
  The launcher fills the required keys around the test's overrides (everything
  shared except `processArguments`; `swap` follows `memory`).
- **Restart policy.** `collector enroll` persists the sharing policy in
  `config.json`. A later start without an explicit override reads the existing
  config and preserves it, exactly like a real restart, so the enrollment
  privacy test proves the policy survives instead of the harness resetting it.
- **Hostname.** Not part of the config schema; the launcher passes
  `--hostname NAME`.
- **Synthetic mode.** `components.collector.synthetic` runs the explicit mock
  machine: no host resource is read, names are prefixed `synthetic`, and
  process detail returns an honest empty table.
- **Enrollment grant.** The launcher passes the grant on the command line for
  the explicit enrollment operation. Production stages it with `rigscope
  install --hub URL --token TOKEN`.
- **TLS.** Trust is pinned per destination: `destinations[].caFile`, used by
  both delivery and enrollment. The launcher can map a test's
  `NODE_EXTRA_CA_CERTS` to that field, but no current runtime test uses the
  Node loopback proxy; the child is spawned **without** `SSL_CERT_FILE`,
  `SSL_CERT_DIR` or `NODE_EXTRA_CA_CERTS` so ambient trust cannot substitute
  for the pinned CA. Direct TLS and trusted-proxy behaviour are covered by
  `internal/hub/server` tests; pinned-CA enrollment and delivery are covered by
  `internal/collector/enroll` and `internal/collector/delivery` tests.

## Hub launch translation

The launcher owns the fixture translation; suites pass logical options:

- host/port → `components.hub.listen`/`port` (port 0 becomes the
  `RIGSCOPE_HUB_PORT=0` ephemeral override).
- database path → `RIGSCOPE_HUB_DATABASE_PATH`; flush cadence →
  `RIGSCOPE_HUB_FLUSH_MS`. The harness asks for 50 ms so ack latency is
  comparable at a 100 ms cadence; production keeps the 1 s durable default.
- configured collectors and enrollment grants → `<stateDir>/collectors.json`,
  imported exactly once.
- browser users → `<stateDir>/users.json`, imported exactly once. Valid
  fixture hashes are re-derived as argon2id hashes of the same public fixture
  password; malformed values pass through so startup validation still fails.
  `browserUsers: false` sets `RIGSCOPE_HUB_DISABLE_BOOTSTRAP=1`, so the hub
  really starts without an account instead of printing an owner password.

Sessions are **durable**: a hub restart keeps unexpired browser sessions, open
sessions are re-adopted by the new process, and logout, account changes, expiry
and eviction still delete the row and close SSE/process interests.

The relay used by delivery tests forwards end-to-end handshake headers but not
`Host`: as a forward proxy it lets the upstream client generate the hub's
authority. Without that, a strict hub correctly answers 421.

## Go collector runtime notes

- `rigscope collector` holds `collector-runtime.lock` inside
  `<stateRoot>/collector/`, the collector-owned subtree, so a second process on
  one state directory fails instead of double-sampling.
- Every log line is one JSON object on stderr (journald keeps the fields). The
  first welcome of a process also prints one plain line on stdout,
  `collector ready: welcomed by hub <url>`, which is what the harness's stdout
  line watcher reads; stdout carries nothing else.
- With no destination or credential the process stays in an "awaiting
  enrollment" dormant state and re-reads its private state every 5 s, so
  enrollment can complete without a restart.
- SIGTERM/SIGINT stop sampling, drain the RAM queue within a bounded 3 s
  window, and exit 0. Records still queued at the deadline are discarded by
  design; the hub reports the hole as missing samples.

## Test table

| File | Test | Coverage notes |
| --- | --- | --- |
| `administration.integration.ts` | packaged administration preserves scoped authorization and revokes existing access immediately | No collector process. |
| `browser.integration.ts` | browser boundary fails closed without users; collector and browser credentials are separate | No collector process. The static export has no Next server layer: unauthenticated documents are the export itself and the client routes to `/login`. |
| `browser.integration.ts` | scoped browser streams filter snapshot/update/reset and logout terminates existing access | No collector process. Also proves durable sessions close an open SSE stream on logout. |
| `browser.integration.ts` | browser login requires exact same origin and rejects bad passwords without a cookie | No collector process. |
| `cadence.integration.ts` | real collector advertises its one hub cadence and applies its local sharing policy | Single hub: exact advertised interval, progressing samples, settled CPU, withheld memory, clean shutdown, last-known cadence after disconnect. |
| `delivery.integration.ts` | delayed acks retain delivered records until the hub is allowed to answer | Relay holds every ack; a collector status shows records in flight, and after release the queue drains with `lastAckAt` set. |
| `delivery.integration.ts` | lost acks are resent on reconnect and the hub commits each record once | Relay drops one ack, the test reads the accepted `(runId, sequence)` identities out of that frame, and a reconnect must resend every one of them. The hub reconciles the full sent set against `sample_receipts`: one receipt per identity, no duplicate commits. |
| `delivery.integration.ts` | a retry response pauses replay and delivery then resumes | Relay injects an `ack` with `retryAfterMs`, holds acks to build replay backlog, proves the first replay batch waits for the delay, then drains. |
| `delivery.integration.ts` | inventory is announced on connect and re-announced after a reconnect | Relay observes `inventory` on connect, the hub live state exposes it, and a forced reconnect produces a second announcement. |
| `delivery.integration.ts` | the collector keeps running across a hub restart and resumes delivery | Same process survives the hub stopping/restarting; sequences advance, `runId` is unchanged and a new `welcome` is forwarded. |
| `delivery.integration.ts` | process detail leases renew past the viewer lease and stop on DELETE | Reads `leaseMs` from the relayed `detail.subscribe` and requires a matching `detail.result` strictly after `subscribe.at + leaseMs`, so ignored renewals fail. DELETE still forwards `detail.cancel` and stops results (HTTP 410 too). |
| `delivery.integration.ts` | process detail leases stop delivering results after the viewer expires | After one result the relay hides every later `detail.subscribe` renewal and the hub's `detail.cancel`, so only the collector's own last forwarded lease can stop it. Requires no result after that lease's expiry plus one interval, and confirms the relay really suppressed the cancel. |
| `durable.integration.ts` | packaged collector: enrollment sharing withholds memory across restart while CPU reaches history | Single-hub privacy/history: CPU reaches persistent history, withheld memory only appears as null points, the restart produces a new `runId`, and the enrollment policy survives the config rebuild. |
| `durable.integration.ts` | packaged real collector: persistent live/history, process interests and hub restart | One hub: live metrics, process interests, persistent history, and recovery after a hub restart with the same state and database. |
| `durable.integration.ts` | default RAM hub loses telemetry on restart while preserving collector enrollment | Omitted mode selects RAM, history and snapshots disappear after restart, and durable enrollment remains. |
| `durable.integration.ts` | the default collector writes no queue file and reports its memory queue | Also checks persistent history serving and that no outbox directory or file appears in the state tree. |
| `hub.integration.ts` | packaged hub: exact health shape, UI route, and collector endpoint HTTP access | Health mode is `go`; the embedded export serves `/`; unknown paths 404 from the export. |
| `hub.integration.ts` | collector endpoint auth: bearer header required, browser Origin upgrades rejected, empty-credential hub still serves health/SSE | No collector process. |
| `hub.integration.ts` | hub config validation: duplicate identities/tokens, missing fields, non-loopback host all fail startup nonzero | No collector process. Also validates malformed and plaintext browser-user bootstraps, and boots with more than fifty machines. |
| `protocol.integration.ts` | collector protocol bounds reject malformed, misordered, oversized and invalid identity messages | Raw WebSocket fixtures exercise the hub parser; no collector executable. |
| `protocol.integration.ts` | hello identity conflict rejects the duplicate and preserves the active session | Raw WebSocket fixtures exercise the hub parser; no collector executable. |
| `protocol.integration.ts` | collector hello deadline closes an authenticated socket that never sends hello | Raw WebSocket fixtures exercise the hub parser; no collector executable. |
| `real-collector.integration.ts` | real collectors enroll into one hub: distinct identities, pre-browser sampling and restart | Two independent collectors, one hub: distinct identities/credentials, sampling before any browser, real CPU/memory, advancing sequences, clean shutdown, restart keeps `collectorId` and changes `runId`. |
| `real-collector.integration.ts` | enrolled hub credentials and consumed grants survive hub restart | Single-hub enrollment, credential persistence and hub restart; bootstrap grants are stored hashed. |
| `shutdown.integration.ts` | hub SIGTERM closes active SSE and WS clients, exits zero and releases its port | No collector process. |
| `shutdown.integration.ts` | hub SIGINT closes active SSE and WS clients, exits zero and releases its port | No collector process. |
| `stream.integration.ts` | mock collector: samples reach SSE snapshot and update with full contract shape; SIGTERM exits zero | Synthetic collector mode. |
| `stream.integration.ts` | SSE reconnect: Last-Event-ID raises a reset snapshot; event ids stay `<hubRunId>:<cursor>` | Hub-only SSE test; no collector executable. |
| `topology.integration.ts` | two collectors map to separate machines on one hub | Two independent single-hub collectors; fan-in is unchanged in 1.0. |
| `topology.integration.ts` | a single destination recovers after the hub is unavailable and later returns | Hub absent → appears → goes away → returns; one collector process survives and delivery resumes with advancing sequences. |

## Removed with the Node prototype

Each row is a test or assertion that only described the Node hub or Node
collector. The behaviour they proved is either gone from 1.0 or is covered by
the Go tests named above.

| Removed | Reason |
| --- | --- |
| `https-boundary.integration.ts` (whole file) | The Node hub's Unix-socket remote boundary does not exist in 1.0. Direct HTTPS, Host/Origin checks, trusted proxy chains, cookies and shutdown are covered by `internal/hub/server` and `internal/hub/security` tests. |
| `tls.integration.ts` (whole file) | Loopback TLS proxy fixture that preserves and forwards the proxy `Host`; the Go hub requires its own bound authority. Direct TLS and trusted-proxy behaviour are covered by `internal/hub/server` tests. |
| `real-collector` — real enrollment and sampling over TLS | Same Node-boundary TLS proxy fixture. Pinned-CA enrollment and delivery are covered by `internal/collector/enroll` and `internal/collector/delivery` TLS tests, and direct hub TLS by `internal/hub/server`. |
| `durable` — memory history mode | The default RAM hub loses telemetry on restart while preserving enrollment; persistent restart coverage explicitly opts in to persistent mode. |
| `durable` — replay a retained canonical SQLite outbox | 1.0 has a bounded RAM queue and explicit gap records; the durable outbox is out of scope. |
| `durable` — multi-hub live/history and durable restart | 1.0 allows exactly one destination. The single-hub persistent history, process-interest and restart assertions are kept in `durable.integration.ts`. |
| `cadence` — per-destination cadence and policy | 1.0 has one cadence and one collector-local sharing policy. |
| `topology` — one collector to two hubs | Multi-hub fan-out is deferred. |
| `topology` — offline destination never blocks healthy delivery | Needs a second destination; a single destination's outage is covered by the recovery test. |
| `browser` — Next server redirect and RSC 401 probes | The static export has no framework server; the API routes still fail closed and are asserted. |
| `hub` — Next delegation for unknown routes | Replaced by the export's own 404 assertion. |
| `hub` — development session lifetime override in production | A Node-only development knob; the Go hub has no such mode. |

Support modules `https.ts`, `remote.ts`, `unix.ts` and `tls-proxy.ts` were
deleted with the suites that used them.

## Relay API (delivery tests)

`tests/runtime/support/relay.ts` is a transparent WebSocket proxy that records
both directions and steers acknowledgements:

| Method | Effect |
| --- | --- |
| `url` | `ws://127.0.0.1:<port>/api/collector/connect` destination for the collector. |
| `holdAcks()` / `releaseAcks()` | Withhold every hub→collector `ack`, then deliver them in order. |
| `dropAcks()` / `forwardAcks()` | Discard acks (a lost ack), or return to normal forwarding. |
| `dropHubTypes(types)` / `forwardHubTypes()` | Hide selected hub→collector message types (for example `detail.subscribe` renewals and `detail.cancel`), or stop hiding them. |
| `injectRetry(ms)` | Send `{type:"ack", accepted:[], rejected:[], retryAfterMs: ms}` to the collector. |
| `disconnect()` | Abruptly close both sockets while staying listening; the collector reconnects. |
| `waitForFrame`, `waitForConnection` | Bounded waits for recorded traffic or a new collector connection. |
| `frames()`, `heldAcks()`, `droppedAcks()`, `suppressedHubFrames()` | Observed traffic and cumulative counters. |

The relay is transparent beyond acknowledgement control: every end-to-end
handshake header the collector sent (including `Origin`, `Authorization`, the
collector-visible `Host` and custom headers) reaches the hub unchanged, and
payload bytes plus the WebSocket text/binary flag are preserved in both
directions, including frames buffered while the upstream connects and acks
held for release. The unit test in `tests/runtime-support.test.ts` covers
those properties directly.

## Observed counts

On 2026-11-12, against one Go binary built from this branch (UI embedded,
concurrency 4): **35 passed, 0 failed, 0 skipped**. The phase-1 "Node hub"
columns and the 14 documented skips are gone with the Node implementations.
