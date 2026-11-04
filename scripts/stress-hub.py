#!/usr/bin/env python3
"""Resource-guarded capacity search around the isolated TypeScript harness.

No production endpoint is used. Each attempt gets a new temporary database.
Run: python3 scripts/stress-hub.py --search collectors --duration-ms 20000
Or:  python3 scripts/stress-hub.py --machines 100 --users 64 --sse 64
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]
GIB = 1024 ** 3
CLOCK = os.sysconf("SC_CLK_TCK")
PAGE = os.sysconf("SC_PAGE_SIZE")
LIMITS = {
    "ackP95Ms": 500, "historyP95Ms": 1000, "eventLoopP99Ms": 200,
    "sseNewestSampleP95Ms": 2000, "minimumCadenceRatio": 0.95,
    "minimumHistoryCadenceRatio": 0.90,
    "minimumAvailableMemoryBytes": 16 * GIB,
    "maximumTestRssBytes": 4 * GIB, "minimumFreeDiskBytes": 10 * GIB,
    "maximumHostCpuPercent": 80, "maximumSwapOutBytes": 256 * 1024 ** 2,
}


def host_sample():
    memory = dict((parts[0].rstrip(":"), int(parts[1]) * 1024)
                  for line in Path("/proc/meminfo").read_text().splitlines()
                  if len(parts := line.split()) >= 2)
    cpu = [int(x) for x in Path("/proc/stat").read_text().splitlines()[0].split()[1:9]]
    vm = dict(line.split() for line in Path("/proc/vmstat").read_text().splitlines())
    disk = os.statvfs(ROOT)
    return {"availableMemoryBytes": memory["MemAvailable"],
            "freeDiskBytes": disk.f_bavail * disk.f_frsize,
            "swapOutBytes": int(vm["pswpout"]) * PAGE,
            "swapInBytes": int(vm["pswpin"]) * PAGE,
            "cpuTotalTicks": sum(cpu), "cpuIdleTicks": cpu[3] + cpu[4],
            "loadAverage": os.getloadavg()}


def process_tree(root_pid):
    procs = {}
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
            procs[int(entry.name)] = {"parent": int(fields[1]),
                "cpuTicks": int(fields[11]) + int(fields[12]),
                "rssBytes": int(fields[21]) * PAGE}
        except (OSError, ValueError, IndexError):
            pass
    members = {root_pid}
    while True:
        added = {pid for pid, p in procs.items() if p["parent"] in members}
        if added.issubset(members):
            break
        members |= added
    return {pid: procs[pid] for pid in members if pid in procs}


def evaluate(case):
    i, h, s, hub = case["ingestion"], case["history"], case["stream"], case["hub"]
    w, f, b = hub["window"], hub["final"], hub["baseline"]
    cfg = case["config"]
    # Window-only durable throughput; startup and drain records are excluded.
    committed = w["store"]["committedRecords"] - b["store"]["committedRecords"]
    expected = cfg["machines"] * case["elapsedMs"] / cfg["sampleIntervalMs"] * cfg["recordsPerBatch"]
    cadence = committed / expected
    history_cadence = h["ok"] / (cfg["viewers"] * case["elapsedMs"] / cfg["historyIntervalMs"])
    reasons = []
    for name, value, threshold in [
        ("ack p95", i["ackLatencyMs"]["p95"], LIMITS["ackP95Ms"]),
        ("history p95", h["latencyMs"]["p95"], LIMITS["historyP95Ms"]),
        ("event loop p99", w["eventLoopDelayMs"]["p99"], LIMITS["eventLoopP99Ms"]),
        ("SSE newest-sample p95", s["normalEventLagMs"]["p95"], LIMITS["sseNewestSampleP95Ms"]),
    ]:
        if value > threshold:
            reasons.append(f"{name} {value} > {threshold} ms")
    if cadence < LIMITS["minimumCadenceRatio"]:
        reasons.append(f"durable sample cadence {cadence:.1%} < 95%")
    if history_cadence < LIMITS["minimumHistoryCadenceRatio"]:
        reasons.append(f"History cadence {history_cadence:.1%} < 90%")
    if (i["recordsAccepted"] != i["recordsSent"] or any(i[k] for k in
            ["recordsRejected", "retryAcks", "unackedLiveRecords", "sendErrors", "backpressureSkips", "reconnectFailures"])):
        reasons.append("collector rejection, retry, loss, backpressure or send error")
    if h["errors"] or h["rateLimited"] or h["truncated"]:
        reasons.append("History error, rate limit or truncation")
    if w["liveClients"] != cfg["sseClients"] or s["snapshots"] != cfg["sseClients"] or s["slowDrops"]:
        reasons.append(f"SSE admission/delivery incomplete ({w['liveClients']}/{cfg['sseClients']})")
    if s["updates"] < cfg["sseClients"] * cfg["durationMs"] / 2000:
        reasons.append("live update rate below one update per two seconds per viewer")
    if w["connectedMachines"] != cfg["machines"]:
        reasons.append("collector connections incomplete")
    if f["store"]["queuedRecords"] or f["store"]["writeFailures"] or f["store"]["retention"]["physicalPressure"]:
        reasons.append("storage queue, write failure or physical pressure")
    return {"pass": not reasons, "reasons": reasons,
        "machines": cfg["machines"], "users": cfg["viewers"], "sseClients": cfg["sseClients"],
        "durationMs": cfg["durationMs"], "committedPerSecond": round(committed / (case["elapsedMs"] / 1000), 1),
        "sseWorkers": cfg.get("sseWorkers", 0), "clientWarmupMs": cfg.get("clientWarmupMs", 0),
        "historyMetricSet": cfg["historyMetricSet"], "historyIntervalMs": cfg["historyIntervalMs"],
        "sampleCadenceRatio": round(cadence, 4), "historyCadenceRatio": round(history_cadence, 4),
        "ackP95Ms": i["ackLatencyMs"]["p95"], "historyP95Ms": h["latencyMs"]["p95"],
        "eventLoopP99Ms": w["eventLoopDelayMs"]["p99"],
        "sseNewestSampleP95Ms": s["normalEventLagMs"]["p95"],
        "hubCpuPercentOneCore": w["cpuPercent"], "hubRssPeakMiB": round(w["rssPeakBytes"] / 1024 ** 2, 1),
        "sseMegabytes": round(s["bytes"] / 1e6, 1)}


def run(args, machines, users, sse, label="probe"):
    stamp = time.strftime("%H%M%S")
    name = f"{stamp}-{label}-m{machines}-u{users}-s{sse}-{args.duration_ms}ms"
    base = args.out / name
    report_path = base.with_suffix(".json")
    command = ["node", "--import", "tsx", "scripts/scale-harness.ts", "--case",
        "fleet-75m-history" if args.broad else "fleet-75m", "--machines", str(machines),
        "--viewers", str(users), "--sse-clients", str(sse), "--sse-workers", str(args.sse_workers),
        "--duration-ms", str(args.duration_ms),
        "--history-interval-ms", str(args.history_interval_ms), "--database-budget-mb", "1024",
        "--json-out", str(report_path)]
    print(f"START {name}", flush=True)
    baseline = previous_host = host_sample()
    started = last_time = time.monotonic()
    previous_procs = {}
    samples, aborted = [], None
    with base.with_suffix(".log").open("w") as log:
        child = subprocess.Popen(command, cwd=ROOT, stdout=log, stderr=log,
                                 start_new_session=True, preexec_fn=lambda: os.nice(10))
        try:
            while child.poll() is None:
                time.sleep(1)
                now, host, procs = time.monotonic(), host_sample(), process_tree(child.pid)
                elapsed = now - last_time
                total_delta = host["cpuTotalTicks"] - previous_host["cpuTotalTicks"]
                host_cpu = 100 * (1 - (host["cpuIdleTicks"] - previous_host["cpuIdleTicks"]) / max(1, total_delta))
                item = {"elapsedSeconds": round(now - started, 2), **host,
                    "hostCpuPercent": round(host_cpu, 2),
                    "testRssBytes": sum(p["rssBytes"] for p in procs.values()),
                    "processes": {str(pid): {**p, "cpuPercentOneCore": round(
                        max(0, p["cpuTicks"] - previous_procs.get(pid, p)["cpuTicks"]) / CLOCK / elapsed * 100, 2)}
                        for pid, p in procs.items()}}
                samples.append(item)
                if host["availableMemoryBytes"] < LIMITS["minimumAvailableMemoryBytes"]:
                    aborted = "available system memory below 16 GiB"
                elif item["testRssBytes"] > LIMITS["maximumTestRssBytes"]:
                    aborted = "test process RSS above 4 GiB"
                elif host["freeDiskBytes"] < LIMITS["minimumFreeDiskBytes"]:
                    aborted = "disk space below 10 GiB"
                elif host["swapOutBytes"] - baseline["swapOutBytes"] > LIMITS["maximumSwapOutBytes"]:
                    aborted = "more than 256 MiB of new system swap out"
                elif len(samples) >= 5 and all(s["hostCpuPercent"] > LIMITS["maximumHostCpuPercent"] for s in samples[-5:]):
                    aborted = "host CPU above 80% for five seconds"
                elif now - started > args.duration_ms / 1000 + 240:
                    aborted = "case exceeded setup/measurement time budget"
                if aborted:
                    break
                previous_host, previous_procs, last_time = host, procs, now
        finally:
            if child.poll() is None:
                os.killpg(child.pid, signal.SIGTERM)
                try:
                    child.wait(timeout=12)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait()
    resource_path = base.with_suffix(".resources.json")
    resource_path.write_text(json.dumps({"command": command, "limits": LIMITS,
        "aborted": aborted, "samples": samples}, indent=2) + "\n")
    result = {"pass": False, "reasons": [aborted or f"harness exit {child.returncode}"],
              "machines": machines, "users": users, "sseClients": sse}
    if report_path.exists() and not aborted and child.returncode == 0:
        result = evaluate(json.loads(report_path.read_text())["cases"][0])
    result.update({"report": os.path.relpath(report_path, ROOT),
        "resources": os.path.relpath(resource_path, ROOT), "wallSeconds": round(time.monotonic() - started, 1),
        "minimumAvailableMemoryGiB": round(min(s["availableMemoryBytes"] for s in samples) / GIB, 1),
        "peakHostCpuPercent": max(s["hostCpuPercent"] for s in samples),
        "peakTestRssMiB": round(max(s["testRssBytes"] for s in samples) / 1024 ** 2, 1),
        "swapOutDeltaBytes": samples[-1]["swapOutBytes"] - baseline["swapOutBytes"]})
    with (args.out / "index.jsonl").open("a") as output:
        output.write(json.dumps(result) + "\n")
    print(json.dumps(result), flush=True)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--search", choices=["collectors", "mixed"])
    parser.add_argument("--machines", type=int, default=100)
    parser.add_argument("--users", type=int, default=1)
    parser.add_argument("--sse", type=int)
    parser.add_argument("--sse-workers", type=int, default=4)
    parser.add_argument("--duration-ms", type=int, default=20000)
    parser.add_argument("--history-interval-ms", type=int, default=1000)
    parser.add_argument("--broad", action="store_true")
    parser.add_argument("--maximum", type=int, default=6400)
    parser.add_argument("--out", type=Path, default=ROOT / f"docs/stress-{time.strftime('%Y-%m-%d')}")
    args = parser.parse_args()
    args.out = args.out.resolve()
    args.out.mkdir(parents=True, exist_ok=True)
    if not args.search:
        run(args, args.machines, args.users, args.sse if args.sse is not None else args.users)
        return
    users = 64 if args.search == "mixed" else args.users
    sse = args.sse if args.sse is not None else (0 if args.search == "collectors" else users)
    low, high, candidate = 0, None, args.machines
    while True:
        result = run(args, candidate, users, sse)
        if result["pass"]:
            low = candidate
            if high is None:
                if candidate >= args.maximum:
                    break
                candidate = min(candidate * 2, args.maximum)
                continue
        else:
            high = candidate
        if low == 0 or high - low <= max(25, low * 0.125):
            break
        candidate = (low + high) // 2
    print(json.dumps({"search": args.search, "highestPassing": low,
                      "lowestFailing": high, "users": users, "sse": sse}), flush=True)


if __name__ == "__main__":
    main()
