/** Storage catalog and flattening for bottleneck metrics. */
import { strict as assert } from "node:assert";
import { describe, it } from "node:test";

import { METRICS, flattenMetrics } from "../src/hub/storage/series";
import type { FlatPoint } from "../src/hub/storage/series";
import { makePayload } from "./storage-fixtures";

function point(points: FlatPoint[], metric: string, resourceId = ""): number | null | undefined {
  return points.find((item) => item.metric === metric && item.resourceId === resourceId)?.value;
}

describe("bottleneck metric catalog", () => {
  it("uses the documented metric names and units", () => {
    assert.deepEqual(
      {
        "cpu.temperature": METRICS["cpu.temperature"].unit,
        "cpu.power": METRICS["cpu.power"].unit,
        "cpu.iowait": METRICS["cpu.iowait"].unit,
        "cpu.pressure": METRICS["cpu.pressure"].unit,
        "memory.usage": METRICS["memory.usage"].unit,
        "memory.pressure.some": METRICS["memory.pressure.some"].unit,
        "memory.pressure.full": METRICS["memory.pressure.full"].unit,
        "io.pressure.some": METRICS["io.pressure.some"].unit,
        "io.pressure.full": METRICS["io.pressure.full"].unit,
        "fs.usage": METRICS["fs.usage"].unit,
        "fs.inodes.usage": METRICS["fs.inodes.usage"].unit,
        "disk.read.latency": METRICS["disk.read.latency"].unit,
        "disk.write.latency": METRICS["disk.write.latency"].unit,
        "disk.utilization": METRICS["disk.utilization"].unit,
      },
      {
        "cpu.temperature": "celsius",
        "cpu.power": "watts",
        "cpu.iowait": "percent",
        "cpu.pressure": "percent",
        "memory.usage": "percent",
        "memory.pressure.some": "percent",
        "memory.pressure.full": "percent",
        "io.pressure.some": "percent",
        "io.pressure.full": "percent",
        "fs.usage": "percent",
        "fs.inodes.usage": "percent",
        "disk.read.latency": "milliseconds",
        "disk.write.latency": "milliseconds",
        "disk.utilization": "percent",
      },
    );
    // The existing per-core history series keeps its resource ids.
    assert.equal(METRICS["cpu.core"].unit, "percent");
  });

  it("maps optional fields and derives memory/filesystem percentages", () => {
    const points = flattenMetrics(
      makePayload({
        cpuTemperature: 61.5,
        cpuPower: 88.25,
        cpuIowait: 1.25,
        cpuPressure: 3.5,
        memoryUsed: 500,
        memoryTotal: 1000,
        memoryPressureSome: 1.5,
        memoryPressureFull: 0.5,
        diskPressureSome: 2.5,
        diskPressureFull: 1.25,
        filesystems: [{ id: "fs:/", used: 20, total: 40, inodesUsed: 25, inodesTotal: 100 }],
        disks: [{ id: "disk:sda", read: 10, write: 20, readLatency: 1.25, writeLatency: 2.5, utilization: 12.5 }],
      }),
    );

    assert.equal(point(points, "cpu.usage"), 10);
    assert.equal(point(points, "cpu.temperature"), 61.5);
    assert.equal(point(points, "cpu.power"), 88.25);
    assert.equal(point(points, "cpu.iowait"), 1.25);
    assert.equal(point(points, "cpu.pressure"), 3.5);
    assert.equal(point(points, "cpu.core", "core:0"), 5);

    assert.equal(point(points, "memory.usage"), 50);
    assert.equal(point(points, "memory.pressure.some"), 1.5);
    assert.equal(point(points, "memory.pressure.full"), 0.5);

    assert.equal(point(points, "io.pressure.some"), 2.5);
    assert.equal(point(points, "io.pressure.full"), 1.25);
    assert.equal(point(points, "fs.usage", "fs:/"), 50);
    assert.equal(point(points, "fs.inodes.usage", "fs:/"), 25);
    assert.equal(point(points, "disk.read.latency", "disk:sda"), 1.25);
    assert.equal(point(points, "disk.write.latency", "disk:sda"), 2.5);
    assert.equal(point(points, "disk.utilization", "disk:sda"), 12.5);
  });

  it("does not derive a percentage from a non-positive total", () => {
    const points = flattenMetrics(
      makePayload({
        memoryUsed: 0,
        memoryTotal: 0,
        filesystems: [{ id: "fs:zero", used: 0, total: 0, inodesUsed: 0, inodesTotal: 0 }],
        disks: [{ id: "disk:idle", readLatency: null, writeLatency: null, utilization: null }],
      }),
    );
    assert.equal(point(points, "memory.usage"), null);
    assert.equal(point(points, "fs.usage", "fs:zero"), null);
    assert.equal(point(points, "fs.inodes.usage", "fs:zero"), null);
    // No completed operations is unknown latency, never 0 ms.
    assert.equal(point(points, "disk.read.latency", "disk:idle"), null);
    assert.equal(point(points, "disk.write.latency", "disk:idle"), null);
    assert.equal(point(points, "disk.utilization", "disk:idle"), null);
  });

  it("omits inode usage when statfs supplied no inode fields at all", () => {
    const points = flattenMetrics(makePayload({ filesystems: [{ id: "fs:none", used: 1, total: 2 }] }));
    assert.equal(point(points, "fs.inodes.usage", "fs:none"), undefined);
  });

  it("breaks previously known optional series when a family becomes unavailable", () => {
    const known = [
      { metric: "cpu.temperature", resourceId: "" },
      { metric: "cpu.power", resourceId: "" },
      { metric: "cpu.iowait", resourceId: "" },
      { metric: "cpu.pressure", resourceId: "" },
      { metric: "memory.pressure.some", resourceId: "" },
      { metric: "memory.pressure.full", resourceId: "" },
      { metric: "fs.usage", resourceId: "fs:/" },
      { metric: "fs.inodes.usage", resourceId: "fs:/" },
      { metric: "io.pressure.some", resourceId: "" },
      { metric: "io.pressure.full", resourceId: "" },
      { metric: "disk.read.latency", resourceId: "disk:sda" },
      { metric: "disk.write.latency", resourceId: "disk:sda" },
      { metric: "disk.utilization", resourceId: "disk:sda" },
    ];
    const points = flattenMetrics(makePayload({ unavailable: { cpu: true, memory: true, disks: true } }), known);
    for (const item of known) {
      const match = points.find((entry) => entry.metric === item.metric && entry.resourceId === item.resourceId);
      assert.ok(match, `${item.metric}/${item.resourceId} should receive a break point`);
      assert.equal(match.value, null, `${item.metric}/${item.resourceId} must break with NULL`);
    }
  });

  it("breaks derived series when an individual filesystem or disk fails", () => {
    const payload = makePayload();
    payload.disks = {status: "ok", filesystems: [{id: "fs:/", status: "unavailable", reason: "permission denied"}],
      devices: [{id: "disk:sda", status: "unavailable", reason: "device reset"}]};
    const points = flattenMetrics(payload);
    for (const metric of ["fs.usage", "fs.inodes.usage"]) assert.equal(point(points, metric, "fs:/"), null);
    for (const metric of ["disk.read.latency", "disk.write.latency", "disk.utilization"])
      assert.equal(point(points, metric, "disk:sda"), null);
    const withoutInodes = flattenMetrics(makePayload({filesystems: [{id: "fs:/", used: 1, total: 2}]}),
      [{metric: "fs.inodes.usage", resourceId: "fs:/"}]);
    assert.equal(point(withoutInodes, "fs.inodes.usage", "fs:/"), null);
  });
});
