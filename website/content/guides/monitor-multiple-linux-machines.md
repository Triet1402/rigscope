# Monitor multiple Linux machines from one dashboard

Use Rigscope to view a Linux workstation, server, and other machines through one self-hosted dashboard. A **hub** serves the dashboard and stores history; a **collector** runs on each monitored machine and sends readings outbound. The browser connects to the hub, so the device you use to view the dashboard needs no collector.

## Start with one hub

Choose a Linux machine that will remain reachable by your collectors. It needs Node.js 26+ with npm and a working systemd user manager. For an initial local installation, run:

```sh
npx --yes rigscope@latest install --boot
```

This starts both a hub and a local collector. Save the generated owner password and open the printed dashboard URL, normally `http://127.0.0.1:8081`. The password is printed only when the account is first created.

Before connecting another machine, configure remote HTTPS access using the [installation guide](/docs/installation/#access-from-another-computer). It includes Tailscale Serve examples for a new hub or an existing localhost installation. Use your own reachable HTTPS hostname in the steps below; `hub.example.com` is a placeholder. A collector on another computer cannot reach the hub through the hub's localhost address.

## Add a collector on another Linux machine

In the hub dashboard, choose **Add machine**, then **New machine**. Create a connection grant. It is usable once and expires within 15 minutes.

On the machine you want to monitor, run:

```sh
npx --yes rigscope@latest install --role collector --boot
~/.local/bin/rigscope connect https://hub.example.com
```

Paste the grant at the private prompt. The collector starts after enrollment. Repeat with a new grant for each machine. Collectors connect outbound and require no incoming ports.

Run `~/.local/bin/rigscope status` on a collector machine to check its services. On the dashboard, check that its readings are arriving. If it cannot connect, check the hub hostname, HTTPS forwarding, certificate, and network access before creating another grant. The [operations reference](/docs/operations/) covers diagnostics and recovery.

## Choose what each machine shares

Managed enrollment shares CPU, memory, swap, GPU, disk, and network readings, plus process details fetched on demand. Network addresses and full process arguments start disabled. Review the [sharing settings](/docs/configuration/#sharing-settings) before enabling additional information.

A collector can send to up to eight hubs, with separate sharing settings for each destination. Hubs have independent users, permissions, history, and alerts; they do not federate into a clustered dashboard.

## Review history and offline machines

Use Fleet to find a machine, Overview for current resources, History for stored readings, and Processes for live process inspection. Process details are never saved to History. A stale receipt and an unavailable sensor are different conditions: a connected machine can lack a particular hardware reading.

Create missing-receipt alerts for machines whose absence matters. Review durations against normal network interruptions and maintenance. Collectors queue unacknowledged samples within bounded storage limits and retry after reconnecting; replayed samples fill History without opening incidents for past threshold crossings. Read the [history and alert behavior](/docs/monitoring/) before relying on notifications.

There is no fixed machine-count guarantee. Capacity depends on the hub, collection intervals, stored series, and workload. Start with your own machines and measure resource use as you expand.
