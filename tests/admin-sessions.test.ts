import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import {
  BrowserSessionStore,
  type SessionEndReason,
} from "../src/hub/auth/sessions";

const NOW = Date.parse("2026-11-02T12:00:00Z");

describe("browser session revocation", () => {
  it("endUser ends every session for one user through the onEnd hook", () => {
    const ended: Array<{ username: string; reason: SessionEndReason }> = [];
    const store = new BrowserSessionStore({
      sweepMs: 0,
      now: () => NOW,
      onEnd: (session, reason) => ended.push({ username: session.username, reason }),
    });
    try {
      const first = store.create("alice", "*");
      const second = store.create("alice", ["m-1"]);
      const bystander = store.create("bob", "*");

      assert.equal(store.endUser("alice"), 2);
      assert.equal(store.size(), 1);
      assert.equal(store.find(first.id), null);
      assert.equal(store.find(second.id), null);
      assert.ok(store.find(bystander.id));
      assert.deepEqual(ended, [
        { username: "alice", reason: "revoked" },
        { username: "alice", reason: "revoked" },
      ]);
      assert.equal(store.endUser("nobody"), 0);
      assert.equal(store.endUser(""), 0);
    } finally {
      store.dispose();
    }
  });

  it("records the admin snapshot for presentation without making it an authorization source", () => {
    const store = new BrowserSessionStore({ sweepMs: 0, now: () => NOW });
    try {
      const admin = store.create("owner", "*", true);
      const viewer = store.create("viewer", "*");
      assert.equal(admin.admin, true);
      assert.equal("admin" in viewer, false);
    } finally {
      store.dispose();
    }
  });
});
