/**
 * Unit tests for the explicit remote listener configuration:
 * HUB_PUBLIC_ORIGIN canonical HTTPS parsing, HUB_SOCKET_PATH structural
 * validation, required-together semantics, and the preserved loopback mode.
 */

import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import {
  HubConfigError,
  parseHubConfig,
  parsePublicOrigin,
  validateUnixSocketPathSyntax,
} from "../src/hub/config";

const ORIGIN = "https://rig.tailnet.example.test";
const SOCKET = "/tmp/rigscope-proxy-test/hub.sock";

describe("hub remote configuration", () => {
  it("keeps loopback TCP mode as the default and unchanged", () => {
    const config = parseHubConfig({});
    assert.equal(config.host, "127.0.0.1");
    assert.equal(config.port, 8081);
    assert.equal(config.remote, null);
    assert.deepEqual(config.collectors, []);
    assert.equal(config.development, false);
  });

  it("requires HUB_PUBLIC_ORIGIN and HUB_SOCKET_PATH together", () => {
    assert.throws(() => parseHubConfig({ HUB_PUBLIC_ORIGIN: ORIGIN }), /must be set together/);
    assert.throws(() => parseHubConfig({ HUB_SOCKET_PATH: SOCKET }), /must be set together/);
    // Empty values remain "not configured", matching the other HUB_ variables.
    assert.equal(parseHubConfig({ HUB_PUBLIC_ORIGIN: "", HUB_SOCKET_PATH: "" }).remote, null);
  });

  it("selects a Unix-only listener with the exact canonical origin", () => {
    const config = parseHubConfig({
      HUB_PUBLIC_ORIGIN: ORIGIN,
      HUB_SOCKET_PATH: SOCKET,
      HUB_COLLECTORS: JSON.stringify([{ collectorId: "c", machineId: "m", token: "t" }]),
      NODE_ENV: "development",
    });
    assert.deepEqual(config.remote, {
      publicOrigin: ORIGIN,
      hostHeader: "rig.tailnet.example.test",
      hostname: "rig.tailnet.example.test",
      port: 443,
      socketPath: SOCKET,
    });
    assert.equal(config.development, true);
    assert.equal(config.collectors.length, 1);
  });

  it("keeps a non-default public port in the exact Host value", () => {
    const remote = parsePublicOrigin("https://rig.tailnet.example.test:8443");
    assert.equal(remote.publicOrigin, "https://rig.tailnet.example.test:8443");
    assert.equal(remote.hostHeader, "rig.tailnet.example.test:8443");
    assert.equal(remote.port, 8443);
  });

  it("rejects HUB_HOST/HUB_PORT in remote mode (no TCP backend)", () => {
    assert.throws(
      () => parseHubConfig({ HUB_PUBLIC_ORIGIN: ORIGIN, HUB_SOCKET_PATH: SOCKET, HUB_PORT: "0" }),
      /not valid in remote mode/,
    );
    assert.throws(
      () => parseHubConfig({ HUB_PUBLIC_ORIGIN: ORIGIN, HUB_SOCKET_PATH: SOCKET, HUB_HOST: "127.0.0.1" }),
      /not valid in remote mode/,
    );
  });

  it("rejects non-HTTPS, non-canonical, and ambiguous public origins", () => {
    const invalid = [
      "http://rig.tailnet.example.test",
      "ws://rig.tailnet.example.test",
      "https://rig.tailnet.example.test/",
      "https://rig.tailnet.example.test/path",
      "https://rig.tailnet.example.test?query=1",
      "https://rig.tailnet.example.test#fragment",
      "https://user:pass@rig.tailnet.example.test",
      "https://rig.tailnet.example.test:443",
      "https://rig.tailnet.example.test:0",
      "HTTPS://RIG.TAILNET.EXAMPLE.TEST",
      "https://Rig.Tailnet.Example.Test",
      "https://rig.tailnet.example.test.",
      "https://",
      "https://rig.tailnet.example.test:99999",
      "https://rig tailnet.example.test",
      "not a url",
    ];
    for (const value of invalid) {
      assert.throws(
        () => parsePublicOrigin(value),
        HubConfigError,
        `${JSON.stringify(value)} must be rejected`,
      );
    }
  });

  it("accepts an ordinary HTTPS origin and a loopback test origin", () => {
    assert.equal(parsePublicOrigin(ORIGIN).hostHeader, "rig.tailnet.example.test");
    assert.equal(
      parsePublicOrigin("https://127.0.0.1:9443").hostHeader,
      "127.0.0.1:9443",
    );
  });

  it("validates Unix socket path syntax without touching the filesystem", () => {
    for (const value of [
      "hub.sock",
      "./hub.sock",
      "~/hub.sock",
      "/tmp/../tmp/hub.sock",
      "/tmp/hub.sock/",
      "/",
      "/tmp/hub\u0000.sock",
      "/tmp/hub\n.sock",
      `/tmp/${"a".repeat(200)}`,
    ]) {
      assert.throws(
        () => validateUnixSocketPathSyntax(value),
        HubConfigError,
        `${JSON.stringify(value)} must be rejected`,
      );
    }
    validateUnixSocketPathSyntax("/tmp/rigscope-private/hub.sock");
    validateUnixSocketPathSyntax("/run/user/1000/rigscope/hub.sock");
    validateUnixSocketPathSyntax("/tmp/rigscope private/hub socket.sock");
  });
});
