# Rigscope

A self-hosted Linux system and process monitor. One Go binary; no Node.js,
Python or external database required.

A **hub** serves the dashboard and stores history. A **collector** reads each
monitored machine and sends readings to one hub. The default install runs both.

## Install and sign in

You need Linux with systemd on `amd64`, `arm64` or `armv7`, plus `curl`
(or GNU `wget`), `tar`, and `sha256sum` (or `shasum`). BusyBox `wget` is unsupported.

```sh
curl -fsSL https://rigscope.dev/install.sh | sh
```

The installer verifies the download, starts user services and pairs the local
collector. Choose an owner password when prompted, or save the generated password
printed once at the end. Open **http://127.0.0.1:8081** and sign in as **owner**.
Lost the password? Run `rigscope reset-password`.

User services start at login. Add `--boot` to start them at boot, or install
machine-wide services with dedicated accounts:

```sh
curl -fsSL https://rigscope.dev/install.sh | sudo sh -s -- --system
```

See [Installation](docs/installation.md) for roles, options and troubleshooting.

## Add a machine

1. Make the hub reachable over HTTPS using [remote access setup](docs/installation.md#remote-access-and-tls).
2. Open **Administration → Add machine** and create a single-use grant.
3. Run the installer command shown there on the new machine. It looks like:

   ```sh
   curl -fsSL https://rigscope.dev/install.sh | sh -s -- \
     --role collector --hub https://hub.example.com --token 'PASTE-THE-GRANT'
   ```

Grants expire after 15 minutes by default. If enrollment fails, create a new one.
Collectors connect outbound and need no inbound port. Plaintext connections are
accepted only on loopback.

## Everyday commands

| Task | Command |
| --- | --- |
| Check services and dashboard URL | `rigscope status` |
| Diagnose a problem | `rigscope doctor` |
| Follow hub logs | `rigscope logs hub --follow` |
| Restart services | `rigscope restart` |
| Upgrade / undo an upgrade | `rigscope upgrade` / `rigscope rollback` |
| See or change settings | `rigscope configure --show`, then for example `rigscope configure --port 9000` |
| Uninstall, keeping data | `rigscope uninstall --keep-data` |

`rigscope uninstall` also deletes saved data. Commands ask before stopping running
services; use `--yes` in scripts. To explicitly select a machine-wide install,
add `--system` (for example, `sudo rigscope status --system`).

## Know before you use it

- Monitoring is read-only. Unavailable readings show a reason rather than zero.
- **History is kept in RAM by default and lost on hub restart.** Enable
  [persistent storage](docs/telemetry-storage.md) for durable summary history.
  Process detail is on demand and never saved.
- Network addresses and full process command lines are withheld by default.
  [Sharing settings](docs/configuration.md#sharing-policy) control what leaves each machine.
- GPU readings depend on the driver; Intel support is partial. NVIDIA junction
  and VRAM temperatures require opt-in setup. See [Monitoring](docs/monitoring.md).
- Version 1.0 does not import data from the old npm/Node 0.2 prototype.
  Follow the [0.2 migration steps](docs/installation.md#upgrading-from-02).

## Documentation

| Start here | Reference |
| --- | --- |
| [Installation](docs/installation.md) | Setup, services, upgrades and troubleshooting |
| [Monitoring and alerts](docs/monitoring.md) | Readings, history, processes and notifications |
| [Configuration](docs/configuration.md) | Settings, TLS, proxies and file locations |
| [Telemetry storage](docs/telemetry-storage.md) | Retention, replay and data gaps |
| [Sensor readings](docs/bottlenecks.md) · [Exports](docs/exports.md) | Interpret and download data |
| [Security](docs/security.md) | Protections and operator responsibilities |
| [Release notes](CHANGELOG.md) · [Architecture](ARCHITECTURE.md) | Changes and technical design |

Maintained by [Triet (@Triet1402)](https://github.com/Triet1402). Licensed under
[AGPL-3.0-only](LICENSE); see the Go dependency notices in
[THIRD_PARTY_NOTICES-go.md](THIRD_PARTY_NOTICES-go.md) and the embedded UI
notices in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Report
vulnerabilities privately using [SECURITY.md](SECURITY.md).

Rigscope 0.2.6 and earlier remain MIT-licensed. See [Licensing](LICENSING.md)
for the transition and corresponding source for modified network deployments.
