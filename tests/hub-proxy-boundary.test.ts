/**
 * Unit tests for the remote request boundary and remote cookie policy:
 * exact Host/Origin enforcement, duplicate-header rejection, forwarded-header
 * replacement, and local-vs-remote session cookie attributes.
 */

import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import type { IncomingMessage } from "node:http";
import {
  checkRemoteRequest,
  normalizeForwardedHeaders,
  singleRequestHeader,
  type RemoteBoundary,
} from "../src/hub/proxy/boundary";
import {
  clearedSessionCookie,
  isTrustedOrigin,
  sessionCookie,
} from "../src/hub/auth/http";
import { REMOTE_SESSION_COOKIE_NAME, SESSION_COOKIE_NAME } from "../src/hub/auth/sessions";

const BOUNDARY: RemoteBoundary = {
  publicOrigin: "https://rig.tailnet.example.test:8443",
  hostHeader: "rig.tailnet.example.test:8443",
};

function request(raw: Record<string, string | string[]>): IncomingMessage {
  const rawHeaders: string[] = [];
  const headers: Record<string, string> = {};
  for (const [name, value] of Object.entries(raw)) {
    const values = Array.isArray(value) ? value : [value];
    for (const item of values) rawHeaders.push(name, item);
    headers[name.toLowerCase()] = values.join(", ");
  }
  return { rawHeaders, headers } as unknown as IncomingMessage;
}

describe("remote request boundary", () => {
  it("rejects ambiguous request targets before Next.js can redirect them", () => {
    for (const url of ["//evil.example/path", "ftp://evil.example/path", "https://", "/\\evil.example/path", "/%5Cevil.example/path", "/path#fragment", "https://evil.example//path", "https://user:pass@evil.example/path"]) {
      const req = request({ host: BOUNDARY.hostHeader });
      req.url = url;
      assert.equal(checkRemoteRequest(req, BOUNDARY)?.status, 400, url);
    }
    for (const url of ["/", "/api/health?probe=1", "https://evil.example/api/health?probe=1"]) {
      const req = request({ host: BOUNDARY.hostHeader });
      req.url = url;
      assert.equal(checkRemoteRequest(req, BOUNDARY), null, url);
    }
  });

  it("requires exactly one exact Host header", () => {
    assert.equal(
      checkRemoteRequest(request({ host: BOUNDARY.hostHeader }), BOUNDARY),
      null,
    );
    assert.equal(
      checkRemoteRequest(request({ host: "rig.tailnet.example.test" }), BOUNDARY)?.status,
      421,
      "a default-port Host is not the configured non-default port",
    );
    assert.equal(
      checkRemoteRequest(request({ host: "evil.example.test:8443" }), BOUNDARY)?.status,
      421,
    );
    assert.equal(checkRemoteRequest(request({}), BOUNDARY)?.status, 400);
    assert.equal(
      checkRemoteRequest(request({ host: [BOUNDARY.hostHeader, "evil.example.test"] }), BOUNDARY)?.status,
      400,
    );
  });

  it("requires a single, exact canonical Origin whenever one is present", () => {
    assert.equal(
      checkRemoteRequest(
        request({ host: BOUNDARY.hostHeader, origin: BOUNDARY.publicOrigin }),
        BOUNDARY,
      ),
      null,
    );
    for (const origin of [
      "https://evil.example.test",
      "http://rig.tailnet.example.test:8443",
      "https://rig.tailnet.example.test",
      "https://rig.tailnet.example.test:9443",
      "null",
    ]) {
      assert.equal(
        checkRemoteRequest(request({ host: BOUNDARY.hostHeader, origin }), BOUNDARY)?.status,
        403,
        `${origin} must be rejected`,
      );
    }
    assert.equal(
      checkRemoteRequest(
        request({ host: BOUNDARY.hostHeader, origin: [BOUNDARY.publicOrigin, "https://evil.example.test"] }),
        BOUNDARY,
      )?.status,
      400,
    );
    // No Origin is allowed for ordinary reads/health/SSE; mutations enforce it
    // separately through isTrustedOrigin.
    assert.equal(checkRemoteRequest(request({ host: BOUNDARY.hostHeader }), BOUNDARY), null);
  });

  it("never lets spoofed forwarded headers satisfy the boundary", () => {
    const spoofed = request({
      host: "evil.example.test",
      "x-forwarded-host": BOUNDARY.hostHeader,
      "x-forwarded-proto": "https",
      forwarded: `host=${BOUNDARY.hostHeader}`,
    });
    assert.equal(checkRemoteRequest(spoofed, BOUNDARY)?.status, 421);
  });

  it("erases incoming Forwarded/X-Forwarded-* headers and sets known values", () => {
    const req = request({
      host: BOUNDARY.hostHeader,
      forwarded: "for=192.0.2.1;host=evil.example.test;proto=http",
      "x-forwarded-for": "192.0.2.1",
      "x-forwarded-host": "evil.example.test",
      "x-forwarded-proto": "http",
      "x-forwarded-port": "1234",
      "x-real-ip": "192.0.2.1",
      "x-custom": "keep",
    });
    (req as unknown as { url: string }).url = "https://evil.example.test/api/health?x=1";
    assert.equal(checkRemoteRequest(req, BOUNDARY), null);
    normalizeForwardedHeaders(req, BOUNDARY);
    assert.equal((req as unknown as { url: string }).url, "/api/health?x=1");
    assert.equal(req.headers.forwarded, undefined);
    assert.equal(req.headers["x-forwarded-for"], undefined);
    assert.equal(req.headers["x-forwarded-port"], undefined);
    assert.equal(req.headers["x-forwarded-proto"], "https");
    assert.equal(req.headers["x-forwarded-host"], BOUNDARY.hostHeader);
    assert.equal(req.headers["x-real-ip"], "192.0.2.1", "unrelated headers are not rewritten");
    assert.equal(req.headers["x-custom"], "keep");
    assert.deepEqual(singleRequestHeader(req, "x-forwarded-proto"), { kind: "single", value: "https" });
    assert.deepEqual(singleRequestHeader(req, "x-forwarded-host"), { kind: "single", value: BOUNDARY.hostHeader });
    assert.deepEqual(singleRequestHeader(req, "forwarded"), { kind: "missing" });
    assert.deepEqual(singleRequestHeader(req, "x-forwarded-for"), { kind: "missing" });
    assert.deepEqual(singleRequestHeader(req, "x-forwarded-port"), { kind: "missing" });
  });

  it("reports duplicate raw headers even when the combined object looks singular", () => {
    const read = singleRequestHeader(request({ host: ["a", "b"] }), "Host");
    assert.deepEqual(read, { kind: "multiple" });
  });

  it("validates exact HTTPS origin for remote browser mutations", () => {
    const rules = {
      mode: "remote" as const,
      publicOrigin: BOUNDARY.publicOrigin,
      hostHeader: BOUNDARY.hostHeader,
    };
    assert.equal(
      isTrustedOrigin(
        request({ host: BOUNDARY.hostHeader, origin: BOUNDARY.publicOrigin }),
        rules,
      ),
      true,
    );
    assert.equal(
      isTrustedOrigin(request({ host: BOUNDARY.hostHeader, origin: "http://rig.tailnet.example.test:8443" }), rules),
      false,
    );
    assert.equal(
      isTrustedOrigin(request({ host: BOUNDARY.hostHeader, origin: "https://rig.tailnet.example.test:9443" }), rules),
      false,
    );
    assert.equal(
      isTrustedOrigin(request({ host: "evil.example.test:8443", origin: BOUNDARY.publicOrigin }), rules),
      false,
    );
    assert.equal(isTrustedOrigin(request({ host: BOUNDARY.hostHeader }), rules), false);
    assert.equal(
      isTrustedOrigin(
        request({ host: BOUNDARY.hostHeader, origin: [BOUNDARY.publicOrigin, BOUNDARY.publicOrigin] }),
        rules,
      ),
      false,
    );
  });
});

describe("remote session cookie policy", () => {
  it("keeps the local plaintext cookie unchanged", () => {
    const cookie = sessionCookie("token", 60);
    assert.ok(cookie.startsWith(`${SESSION_COOKIE_NAME}=token;`));
    assert.match(cookie, /HttpOnly/);
    assert.match(cookie, /SameSite=Strict/);
    assert.match(cookie, /Path=\//);
    assert.match(cookie, /Max-Age=60/);
    assert.equal(/Secure/i.test(cookie), false);
    assert.equal(/Domain=/i.test(cookie), false);
    assert.ok(clearedSessionCookie().startsWith(`${SESSION_COOKIE_NAME}=;`));
  });

  it("uses a Secure host-only __Host- cookie with matching deletion attributes", () => {
    const cookie = sessionCookie("token", 60, true);
    assert.ok(cookie.startsWith(`${REMOTE_SESSION_COOKIE_NAME}=token;`));
    assert.match(cookie, /Secure/);
    assert.match(cookie, /HttpOnly/);
    assert.match(cookie, /SameSite=Strict/);
    assert.match(cookie, /Path=\//);
    assert.match(cookie, /Max-Age=60/);
    assert.equal(/Domain=/i.test(cookie), false, "__Host- cookies must not carry Domain");

    const cleared = clearedSessionCookie(true);
    assert.ok(cleared.startsWith(`${REMOTE_SESSION_COOKIE_NAME}=;`));
    assert.match(cleared, /Secure/);
    assert.match(cleared, /HttpOnly/);
    assert.match(cleared, /SameSite=Strict/);
    assert.match(cleared, /Path=\//);
    assert.match(cleared, /Max-Age=0/);
    assert.equal(/Domain=/i.test(cleared), false);
  });
});
