# Monitor multiple Linux machines from one dashboard

Use Rigscope to view a Linux workstation, server, and other machines through one self-hosted dashboard. A **hub** serves the dashboard and stores history; a **collector** runs on each monitored machine and sends readings outbound. The browser connects to the hub, so the device you use to view the dashboard needs no collector.

## Start with one hub

Choose a Linux machine that will remain reachable by your collectors. Rigscope 1.0 is one static Go binary and needs Linux with systemd on `amd64`, `arm64`, or `armv7`; there is no Node.js, npm, Python, or database server. For an initial local installation, run:

```sh
curl -fsSL https://rigscope.dev/install.sh | sh
```

To read the installer before running it:

```sh
curl -fsSLO https://rigscope.dev/install.sh && less install.sh && sh install.sh
```

This starts a hub and a local collector. The installer creates the `owner` account and asks you to choose a password. Press Enter for a generated password, printed once in the terminal. Then open `http://127.0.0.1:8081` and sign in as `owner`.

Before connecting another machine, configure remote HTTPS access using the [installation guide](/docs/installation/#remote-access-and-tls). Use your own reachable HTTPS hostname in the steps below; `hub.example.com` is a placeholder. A collector on another computer cannot reach the hub through the hub's localhost address.

## Add a collector on another Linux machine

In the hub dashboard, open **Administration → Add machine** and create a grant. The grant is single-use, expires after 15 minutes by default, and is shown once.

On the machine you want to monitor, run the command shown there. It has this shape:

```sh
curl -fsSL https://rigscope.dev/install.sh | sh -s -- \
  --role collector \
  --hub https://hub.example.com \
  --token 'PASTE-THE-GRANT'
```

`install` normalizes the hub address to its collector WebSocket endpoint. For a root-owned system collector, add `sudo` and `--system`. The collector connects outbound and requires no incoming ports. Repeat with a new grant for each machine.

Run `rigscope status` on a collector machine to check its services. On the dashboard, check that its readings are arriving. If it cannot connect, check the hub hostname, HTTPS forwarding, certificate, and network access before creating another grant. The [installation troubleshooting](/docs/installation/#troubleshooting) covers diagnostics and recovery.

## Choose what each machine shares

A collector applies one local sharing policy before anything is queued. CPU, memory, swap, GPU, disk, and network readings, plus on-demand process detail, are shared by default. Network addresses and full command lines are withheld by default. Review the [sharing settings](/docs/configuration/#sharing-policy) before enabling additional information.

One collector talks to one hub in 1.0. Hubs have independent users, permissions, history, and alerts; they do not federate into a clustered dashboard.

## Review history and offline machines

Use Fleet to find a machine, Overview for current resources, History for stored readings, and Processes for live process inspection. Process details are never saved to History. A stale receipt and an unavailable sensor are different conditions: a connected machine can lack a particular hardware reading.

Create missing-receipt alerts for machines whose absence matters. Review durations against normal network interruptions and maintenance. Collectors buffer unacknowledged samples in a bounded queue and retry after reconnecting. A crash can lose queued samples, which the hub marks as unknown loss; replayed samples fill History without opening incidents for past threshold crossings. Read the [history and alert behavior](/docs/monitoring/) before relying on notifications.

There is no fixed machine-count guarantee. Capacity depends on the hub, collection intervals, stored series, and workload. Start with your own machines and measure resource use as you expand.
