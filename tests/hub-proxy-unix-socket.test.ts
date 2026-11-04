/**
 * Unit tests for the private Unix-socket lifecycle: private parent directory,
 * occupied/symlinked path rejection, 0600 socket creation, ownership-safe
 * cleanup, and startup-failure abandonment.
 */

import { strict as assert } from "node:assert";
import {
  chmodSync,
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { createServer, type Server } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";
import {
  abandonUnixSocket,
  prepareUnixSocketPath,
  RemoteSocketError,
  removeOwnedUnixSocket,
  takeUnixSocketOwnership,
} from "../src/hub/proxy/unix-socket";

function tempRoot(): string {
  return mkdtempSync(join(tmpdir(), "rigscope-unix-socket-"));
}

async function listenOn(path: string): Promise<Server> {
  const server = createServer();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(path, () => resolve());
  });
  return server;
}

async function close(server: Server): Promise<void> {
  await new Promise<void>((resolve) => server.close(() => resolve()));
}

describe("private Unix socket lifecycle", () => {
  it("creates a private directory and a 0600 socket, then removes its own socket", async () => {
    const root = tempRoot();
    const parent = join(root, "state");
    const socketPath = join(parent, "hub.sock");
    try {
      prepareUnixSocketPath(socketPath);
      assert.equal(lstatSync(parent).isDirectory(), true);
      assert.equal(lstatSync(parent).mode & 0o777, 0o700);

      const server = await listenOn(socketPath);
      const identity = takeUnixSocketOwnership(socketPath);
      const stat = lstatSync(socketPath);
      assert.equal(stat.isSocket(), true);
      assert.equal(stat.mode & 0o777, 0o600);

      // Explicit ownership-safe removal while the listener still owns the
      // path (Node also unlinks a bound Unix socket on server.close).
      assert.equal(removeOwnedUnixSocket(socketPath, identity), true);
      assert.equal(existsSync(socketPath), false);
      await close(server);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("refuses an occupied regular file without deleting it", () => {
    const root = tempRoot();
    const parent = join(root, "state");
    const socketPath = join(parent, "hub.sock");
    try {
      mkdirSync(parent, { mode: 0o700 });
      writeFileSync(socketPath, "do not delete", { mode: 0o600 });
      assert.throws(() => prepareUnixSocketPath(socketPath), RemoteSocketError);
      assert.equal(existsSync(socketPath), true);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("refuses a symlinked socket path", () => {
    const root = tempRoot();
    const parent = join(root, "state");
    const target = join(parent, "target");
    const socketPath = join(parent, "hub.sock");
    try {
      mkdirSync(parent, { mode: 0o700 });
      writeFileSync(target, "target", { mode: 0o600 });
      symlinkSync(target, socketPath);
      assert.throws(() => prepareUnixSocketPath(socketPath), /symlink/);
      assert.equal(lstatSync(socketPath).isSymbolicLink(), true);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("refuses a non-private parent directory", () => {
    const root = tempRoot();
    const parent = join(root, "state");
    try {
      mkdirSync(parent, { mode: 0o700 });
      chmodSync(parent, 0o755);
      assert.throws(
        () => prepareUnixSocketPath(join(parent, "hub.sock")),
        /must be private/,
      );
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("refuses a symlinked parent directory", () => {
    const root = tempRoot();
    const real = join(root, "real");
    const link = join(root, "link");
    try {
      mkdirSync(real, { mode: 0o700 });
      symlinkSync(real, link);
      assert.throws(
        () => prepareUnixSocketPath(join(link, "hub.sock")),
        /real directory/,
      );
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("never removes a path that was replaced after the socket was created", async () => {
    const root = tempRoot();
    const socketPath = join(root, "hub.sock");
    try {
      prepareUnixSocketPath(socketPath);
      const server = await listenOn(socketPath);
      const identity = takeUnixSocketOwnership(socketPath);
      await close(server);
      // Node unlinks the bound path on close; simulate a replacement.
      writeFileSync(socketPath, "replacement", { mode: 0o600 });
      assert.equal(removeOwnedUnixSocket(socketPath, identity), false);
      assert.equal(existsSync(socketPath), true);
      assert.equal(lstatSync(socketPath).isFile(), true);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("abandons only a socket file after a startup failure", async () => {
    const root = tempRoot();
    const socketPath = join(root, "hub.sock");
    try {
      prepareUnixSocketPath(socketPath);
      const server = await listenOn(socketPath);
      takeUnixSocketOwnership(socketPath);
      assert.equal(lstatSync(socketPath).isSocket(), true);
      // Startup-failure path runs while this process still owns the listener.
      abandonUnixSocket(socketPath);
      assert.equal(existsSync(socketPath), false);
      await close(server);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
