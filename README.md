# Rigscope

Your machines, in view. A self-hosted Linux system and process monitor.

View CPU, memory, GPU, disk and network readings, browse History, inspect running
processes and set alerts. Run a hub for the dashboard and a collector on each
machine you want to monitor. The default installation runs both on this computer.

## Install

Requires **Linux, Node.js 26+ with npm, and systemd** for background services.

```sh
npx --yes rigscope@latest install --boot
```

Save the generated `owner` password and open the printed dashboard URL
(normally `http://127.0.0.1:8081`). The services keep running after you close the
terminal. `--boot` starts them at boot; omit it to start at login instead.

Add `~/.local/bin` to PATH to use `rigscope`. You can also run
`~/.local/bin/rigscope` directly. No global npm install or source checkout is needed.

To try Rigscope in the terminal before installing services:

```sh
npx --yes rigscope@latest try
```

The trial uses separate data and stops with Ctrl+C.

## Use it

| Task | Command or guide |
| --- | --- |
| Check services and the dashboard URL | `rigscope status` |
| Follow hub logs | `rigscope logs hub --follow` |
| Upgrade from npm | `rigscope upgrade` |
| Stop or start the installation | `rigscope stop` / `rigscope start` |
| Open the dashboard through Tailscale HTTPS | [Set up remote access](docs/installation.md#access-from-another-computer) |
| Monitor another machine | [Add a collector](docs/installation.md#add-machines-and-hubs) |
| Install a downloaded package | [Install from an archive](docs/installation.md#install-from-an-archive) |

Each collector connects outbound to up to eight hubs. Each hub has its own users,
permissions, history and alerts. Collectors need no incoming ports.

## What to expect

Rigscope is read-only: it does not kill processes, change priorities or control
services on monitored machines. Missing sensor readings stay unavailable.
NVIDIA and available AMD GPU counters are supported; Intel GPU readings are
partial. See the [hardware notes](docs/installation.md#hardware).

History is stored in SQLite. Process details are fetched while you view them and
are never saved to History. See [Monitoring and alerts](docs/monitoring.md) for
retention, sharing, freshness and notification behavior.

## Documentation

| Guide | Covers |
| --- | --- |
| [Installation](docs/installation.md) | npm, HTTPS, collectors, upgrades and hardware |
| [Operations](docs/operations.md) | Diagnostics, backups, recovery and archive deployment |
| [System installation](docs/system-installation.md) | Dedicated service accounts and system units |
| [Configuration reference](docs/configuration.md) | Environment variables and source enrollment |
| [Monitoring and alerts](docs/monitoring.md) | Data limits, sharing, users and webhooks |
| [Sensor readings](docs/bottlenecks.md) / [Exports](docs/exports.md) | Reading meanings and downloads |
| [Development](docs/development.md) / [Architecture](ARCHITECTURE.md) / [Design](DESIGN.md) | Source setup and implementation references |
| [Releasing](docs/releasing.md) / [Release notes](CHANGELOG.md) | Publishing checks and version changes |
| [Scale validation](docs/scale-validation.md) | Synthetic measurements on one host; no capacity guarantee |

Published on [npm](https://www.npmjs.com/package/rigscope).
Maintained by [Triet (@Triet1402)](https://github.com/Triet1402).
Licensed under [MIT](LICENSE); see [dependency notices](THIRD_PARTY_NOTICES.md).
Report vulnerabilities privately using [SECURITY.md](SECURITY.md).
