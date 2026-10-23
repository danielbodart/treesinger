import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { lstat, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { health, listen, start, systemdNotify, type Listening, type Notify } from "../src/serve.ts";
import { harness, validSeed } from "./support/harness.ts";

// Short on purpose: a unix socket path has to fit in 108 bytes.
let dir: string;
let socketPath: string;
const opened: Listening[] = [];

beforeEach(async () => {
    dir = await mkdtemp(join(tmpdir(), "ts-"));
    socketPath = join(dir, "treesinger.sock");
});

afterEach(async () => {
    await Promise.all(opened.splice(0).map((listening) => listening.stop()));
    await rm(dir, { recursive: true, force: true });
});

/** A broker on real listeners, its upstream the usual fake. */
async function broker(config: { socketPath?: string; port?: number; seeded?: boolean } = {}) {
    const h = harness({ seed: config.seeded ? validSeed : undefined, config: { socketPath: config.socketPath, port: config.port } });
    const listening = await listen(h.config, h.c.app);
    opened.push(listening);
    return { ...h, listening };
}

/** Leave a socket at `path` the way a crash does: its broker SIGKILLed mid-serve. */
async function stale(path: string): Promise<void> {
    const child = Bun.spawn([process.execPath, "-e", `Bun.serve({ unix: ${JSON.stringify(path)}, fetch: () => new Response() }); console.log("up");`], {
        stdout: "pipe",
    });
    for await (const _ of child.stdout) break; // "up": bound
    child.kill("SIGKILL");
    await child.exited;
}

const overSocket = (path: string, init: RequestInit = {}) => fetch(`http://treesinger${path}`, { ...init, unix: socketPath });

describe("unix socket listener", () => {
    test("serves the API: health 503 and not_logged_in before any login", async () => {
        await broker({ socketPath });

        const healthRes = await overSocket("/health");
        expect(healthRes.status).toBe(503);
        expect(await healthRes.json()).toEqual({ status: "unauthenticated" });

        const session = await overSocket("/v1/session", { method: "POST" });
        expect(session.status).toBe(401);
        expect(await session.json()).toEqual({ error: "not_logged_in" });
    });

    test("is mode 0660 whatever the umask", async () => {
        const previous = process.umask(0o022);
        try {
            await broker({ socketPath });
        } finally {
            process.umask(previous);
        }
        expect((await stat(socketPath)).mode & 0o777).toBe(0o660);
    });

    test("there is no TCP listener when only a socket is configured", async () => {
        const { listening } = await broker({ socketPath });
        expect(listening.urls).toEqual([`unix:${socketPath}`]);
    });

    test("a stale socket left by a dead broker is replaced", async () => {
        await stale(socketPath);
        expect((await lstat(socketPath)).isSocket()).toBe(true);

        await broker({ socketPath });

        expect((await overSocket("/health")).status).toBe(503);
    });

    test("a live socket is not stolen from the broker answering on it", async () => {
        await broker({ socketPath });
        const second = harness({ config: { socketPath, port: undefined } });

        await expect(listen(second.config, second.c.app)).rejects.toThrow("already being served");
        expect((await overSocket("/health")).status).toBe(503); // the first still answers
    });

    test("anything that is not a socket is refused and left intact", async () => {
        await writeFile(socketPath, "someone's file");
        const h = harness({ config: { socketPath, port: undefined } });

        await expect(listen(h.config, h.c.app)).rejects.toThrow("not a socket");
        expect(await readFile(socketPath, "utf8")).toBe("someone's file");
    });

    test("stopping removes the socket", async () => {
        const { listening } = await broker({ socketPath });
        await listening.stop();
        await expect(lstat(socketPath)).rejects.toThrow();
    });
});

describe("socket and TCP together", () => {
    test("both answer", async () => {
        const { listening } = await broker({ socketPath, port: 0, seeded: true });

        expect(listening.urls).toHaveLength(2);
        expect(listening.urls[0]).toBe(`unix:${socketPath}`);
        expect((await overSocket("/health")).status).toBe(200);
        expect((await fetch(`${listening.urls[1]}/health`)).status).toBe(200);
    });

    test("a TCP bind failure closes the socket it already opened", async () => {
        const blocker = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: () => new Response() });
        try {
            const h = harness({ config: { socketPath, port: blocker.port } });
            await expect(listen(h.config, h.c.app)).rejects.toThrow();
            await expect(lstat(socketPath)).rejects.toThrow();
        } finally {
            blocker.stop(true);
        }
    });
});

describe("treesinger health", () => {
    test("probes the socket when one is configured", async () => {
        const { c } = await broker({ socketPath, seeded: true });
        expect(await health(c)).toBe(0);
    });

    test("is unhealthy over the socket when no one has logged in", async () => {
        const { c } = await broker({ socketPath });
        expect(await health(c)).toBe(1);
    });

    test("probes loopback TCP when the listener binds every address", async () => {
        const { listening } = await broker({ port: 0, seeded: true });
        const port = Number(new URL(listening.urls[0]!).port);
        const { c } = harness({ seed: validSeed, config: { port, hostname: "0.0.0.0" } });
        expect(await health(c)).toBe(0);
    });

    test("fails when nothing is listening", async () => {
        const { c } = harness({ config: { socketPath } });
        expect(await health(c)).toBe(1);
    });
});

describe("readiness", () => {
    test("is signalled once, after the socket is bound", async () => {
        const calls: boolean[] = [];
        const notify: Notify = async () => {
            calls.push((await lstat(socketPath)).isSocket());
        };
        const { c } = harness({ config: { socketPath, port: undefined } });

        opened.push(await start(c, notify, { NOTIFY_SOCKET: "/run/systemd/notify" }));

        expect(calls).toEqual([true]);
    });

    test("a failed notify does not leave the broker listening", async () => {
        const { c } = harness({ config: { socketPath, port: undefined } });
        const failing: Notify = async () => {
            throw new Error("no systemd");
        };

        await expect(start(c, failing)).rejects.toThrow("no systemd");
        await expect(lstat(socketPath)).rejects.toThrow();
    });
});

describe("systemdNotify", () => {
    test("does nothing outside systemd", async () => {
        await systemdNotify({}); // no NOTIFY_SOCKET: nothing spawned, nothing thrown
    });

    test("fails loudly when systemd's socket cannot be reached", async () => {
        await expect(systemdNotify({ NOTIFY_SOCKET: join(dir, "missing"), PATH: process.env.PATH })).rejects.toThrow();
    });
});
