# Development

Rigscope 1.0 is a Go program with a React/TypeScript UI. Go is the runtime;
Node is only needed to build the static UI export and to run the TypeScript
tests. There is no Node hub, Node collector or npm-installed Rigscope.

See [Architecture](../ARCHITECTURE.md), the
[rewrite plan](rewrite/PLAN.md) and the
[runtime test matrix](rewrite/test-matrix.md) for the system and its tests.

## Requirements

| | |
| --- | --- |
| Go | 1.27 (`go.mod`), Linux |
| Node | 26 or newer for the UI build and tests (the runtime harness uses Node 26's `crypto.argon2Sync`) |
| Tools | `gofmt`, `go`, `npm`, GNU tar, gzip, sha256sum for release work |

## First checkout

```sh
npm ci                 # UI/test dependencies only
go mod download

npm run typecheck      # browser/contract TypeScript
npm test               # UI, contract and harness-support unit tests
go vet ./...
go test ./...          # add -race for the full gate
```

`npm test` runs `tests/*.test.ts` through Node's test runner (four files at a
time; `RIGSCOPE_TEST_CONCURRENCY` accepts 1–16). It does not start the Go
binary. The runtime suites are separate.

## Full stack locally

Build the UI, embed it and build the binary once:

```sh
npm run build:ui
go generate ./internal/hub/webui
go build -o ./rigscope ./cmd/rigscope
```

Then run a hub and a collector against throwaway state directories. This is the
same shape the runtime harness uses; nothing touches an installed service.

```sh
root=$(mktemp -d)
mkdir -p "$root/hub" "$root/collector"

cat >"$root/hub/config.json" <<'JSON'
{
  "version": 1,
  "components": {
    "hub": {
      "enabled": true,
      "listen": "127.0.0.1",
      "port": 8081,
      "origin": "",
      "tlsCert": "",
      "tlsKey": "",
      "trustedProxies": []
    },
    "collector": {
      "enabled": false,
      "destinations": [],
      "samplingInterval": "2s",
      "sharing": {
        "cpu": true, "memory": true, "swap": true, "gpus": true,
        "disks": true, "network": true, "networkAddresses": false,
        "processes": true, "processArguments": false
      },
      "hardware": { "gpuRegisters": false }
    }
  }
}
JSON

./rigscope hub --config "$root/hub/config.json" --state-dir "$root/hub"
```

In this direct development startup, the first hub start creates the `owner`
account and prints its password once to the terminal. Managed installations
instead set up the account during `rigscope install`. Open
<http://127.0.0.1:8081>, sign in, and use **Administration → Add machine** to
create a grant. Then enroll and start a collector in another terminal:

```sh
cat >"$root/collector/config.json" <<'JSON'
{
  "version": 1,
  "components": {
    "hub": {
      "enabled": false,
      "listen": "127.0.0.1", "port": 8081, "origin": "",
      "tlsCert": "", "tlsKey": "", "trustedProxies": []
    },
    "collector": {
      "enabled": true,
      "destinations": [{ "url": "ws://127.0.0.1:8081/api/collector/connect" }],
      "samplingInterval": "2s",
      "sharing": {
        "cpu": true, "memory": true, "swap": true, "gpus": true,
        "disks": true, "network": true, "networkAddresses": false,
        "processes": true, "processArguments": false
      },
      "hardware": { "gpuRegisters": false }
    }
  }
}
JSON

./rigscope collector enroll \
  --config "$root/collector/config.json" \
  --state-dir "$root/collector" \
  --grant 'PASTE-THE-GRANT'
./rigscope collector --config "$root/collector/config.json" --state-dir "$root/collector"
```

Use `--hostname NAME` on the collector for a stable display name. SIGTERM and
SIGINT drain the RAM queue within a bounded window and exit zero.

## UI-only iteration

Every UI change needs the export rebuilt and re-embedded before the Go hub
serves it:

```sh
npm run build:ui
go generate ./internal/hub/webui
go build -o ./rigscope ./cmd/rigscope
```

`npx next dev` starts the UI alone on port 3000 for visual or component work.
Its `/api/*` requests have no hub behind them there, so it is not a full-stack
replacement; use the build-and-embed loop above for anything that reads live or
history data.

## Runtime harness

The runtime suites exercise the Go binary only. `RIGSCOPE_TEST_GO_BINARY` is
required and must be an absolute path:

```sh
npm run build:ui
go generate ./internal/hub/webui
go build -o "$PWD/rigscope" ./cmd/rigscope

RIGSCOPE_TEST_GO_BINARY="$PWD/rigscope" npm run test:runtime
```

`RIGSCOPE_RUNTIME_TEST_CONCURRENCY` accepts 1–16 (default 4). The suites start
isolated loopback hubs and collectors in temporary state directories; they never
touch an installed service. See
[the test matrix](rewrite/test-matrix.md) for what each file covers and which
Node-prototype tests were removed.

## Release artifacts

The reproducible release build embeds the UI, cross-compiles amd64/arm64/armv7,
generates both notices files and writes checksums:

```sh
sh scripts/release/build.sh 1.0.0
sh scripts/release/verify.sh 1.0.0
```

`npm run notices` regenerates `THIRD_PARTY_NOTICES.md` for the browser bundle
from the installed production `node_modules`; it needs no network. The Go
notices are generated by `scripts/release/notices.sh`. The remaining manual
release steps are in [releasing-v1.md](rewrite/releasing-v1.md).

## Style

- `gofmt`, `go vet ./...` and `go test ./...` pass on every commit.
- Parsers are pure functions over bytes or an `fs.FS` so they test against
  `testdata/` without root or real hardware.
- Errors are values; no panics across a sampling boundary, no package-level
  mutable state.
- Keep the TypeScript contract modules browser-safe: no Node built-ins and no
  server code imported by `src/app` or `src/components`.

## Settings usability

See the [settings access audit](settings-audit.md) for implemented CLI and hub
UI controls, the remaining destination-enrollment improvement, and service
management constraints.

## Licensing and source builds

Project-owned sources are AGPL-3.0-only; third-party code retains its licenses.
The release source archive includes vendored Go modules and the browser package
sources. See [Licensing](../LICENSING.md). Build a modified network deployment
with `NEXT_PUBLIC_RIGSCOPE_SOURCE_URL` pointing to its corresponding source.
