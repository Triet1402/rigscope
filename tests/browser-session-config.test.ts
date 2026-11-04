import assert from "node:assert/strict";
import test from "node:test";
import { parseDevelopmentSessionTtl } from "../src/hub/auth/config";
import { DEFAULT_SESSION_TTL_MS } from "../src/hub/auth/sessions";

test("development expiry probes keep the fixed twelve-hour default", () => {
  assert.equal(parseDevelopmentSessionTtl({}), DEFAULT_SESSION_TTL_MS);
  assert.equal(parseDevelopmentSessionTtl({ NODE_ENV: "development" }), DEFAULT_SESSION_TTL_MS);
  assert.equal(parseDevelopmentSessionTtl({ NODE_ENV: "production", HUB_DEV_SESSION_TTL_MS: "" }), DEFAULT_SESSION_TTL_MS);
});

test("only development may shorten the fixed session lifetime", () => {
  for (const value of ["1000", "3000", "43200000"]) {
    assert.equal(parseDevelopmentSessionTtl({ NODE_ENV: "development", HUB_DEV_SESSION_TTL_MS: value }), Number(value));
  }
  for (const NODE_ENV of [undefined, "production", "test"]) {
    assert.throws(() => parseDevelopmentSessionTtl({ NODE_ENV, HUB_DEV_SESSION_TTL_MS: "3000" }), /only allowed in development/);
  }
});

test("development expiry probe lifetime is bounded and strictly parsed", () => {
  for (const value of ["0", "999", "43200001", "-1000", "1000.5", "1e3", " 1000", "1000\n", "01000", "NaN", "Infinity", "9007199254740992"]) {
    assert.throws(() => parseDevelopmentSessionTtl({ NODE_ENV: "development", HUB_DEV_SESSION_TTL_MS: value }), /must be an integer/);
  }
});
