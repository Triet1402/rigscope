import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { type Socket as TcpSocket, connect as netConnect } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import tls from "node:tls";
import { registerCleanup } from "./cleanup";
import { ROOT } from "./config";
import { HubController } from "./fixtures";

export type TlsProxy = { port: number; caPem: string; close(): Promise<void> };

export async function startTlsProxy(hub: HubController): Promise<TlsProxy | null> {
  const which = spawnSync("openssl", ["version"], { encoding: "utf8" });
  if (which.error !== undefined || which.status !== 0) return null;

  const dir = mkdtempSync(join(tmpdir(), "rigscope-spike-tls-"));
  const keyPath = join(dir, "key.pem");
  const certPath = join(dir, "cert.pem");
  try {
    const generated = spawnSync(
      "openssl",
      [
        "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-days", "2", "-nodes",
        "-subj", "/CN=localhost",
        "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1",
        "-keyout", keyPath, "-out", certPath,
      ],
      { encoding: "utf8", cwd: ROOT },
    );
    if (generated.status !== 0 || !existsSync(certPath)) {
      try {
        rmSync(dir, { recursive: true, force: true });
      } catch {
        /* temp cleanup best-effort */
      }
      return null;
    }
    const keyPem = readFileSync(keyPath);
    const caPem = readFileSync(certPath, "utf8");

    type Pair = { tlsSocket: tls.TLSSocket; upstream: TcpSocket; teardown(): void };
    const sockets = new Set<Pair>();
    const server = tls.createServer({ key: keyPem, cert: caPem }, (tlsSocket) => {
      const upstream = netConnect({ host: "127.0.0.1", port: hub.port });
      const pair: Pair = { tlsSocket, upstream, teardown: () => undefined };
      pair.teardown = (): void => {
        tlsSocket.destroy();
        upstream.destroy();
        sockets.delete(pair);
      };
      tlsSocket.on("error", pair.teardown);
      tlsSocket.on("close", pair.teardown);
      upstream.on("error", pair.teardown);
      upstream.on("close", pair.teardown);
      // Raw TCP forwarding preserves SSE streaming and WS upgrade bytes end to end.
      tlsSocket.pipe(upstream);
      upstream.pipe(tlsSocket);
      sockets.add(pair);
    });
    await new Promise<void>((resolve) => server.listen({ host: "127.0.0.1", port: 0 }, () => resolve()));
    const port = (server.address() as { port: number }).port;

    const cleanupAll = async (): Promise<void> => {
      for (const pair of [...sockets]) pair.teardown();
      await new Promise<void>((resolve) => server.close(() => resolve()));
      try {
        rmSync(dir, { recursive: true, force: true });
      } catch {
        /* temp dir cleanup is best-effort */
      }
    };
    registerCleanup(cleanupAll);
    return { port, caPem, close: cleanupAll };
  } catch {
    try {
      rmSync(dir, { recursive: true, force: true });
    } catch {
      /* best-effort */
    }
    return null;
  }
}
