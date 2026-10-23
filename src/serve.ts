import { chmod, lstat, unlink } from "node:fs/promises";
import type { Config } from "./config.ts";
import type { Container } from "./container.ts";
import { version } from "./version.ts";

type Listeners = Pick<Config, "socketPath" | "port" | "hostname">;
type Fetch = (request: Request) => Response | Promise<Response>;

/** The listeners `serve` opened: what to log, and how to close them. */
export interface Listening {
    /** `unix:/path` and/or `http://host:port`, in that order. */
    urls: string[];
    stop(): Promise<void>;
}

/**
 * Open every configured listener — the unix socket first, then TCP — and
 * resolve once all of them are bound. A failure closes whatever did open, so a
 * half-started broker never lingers.
 */
export async function listen(config: Listeners, fetch: Fetch): Promise<Listening> {
    const servers: ReturnType<typeof Bun.serve>[] = [];
    const urls: string[] = [];
    const stop = async () => {
        await Promise.all(servers.map((server) => server.stop(true)));
    };
    try {
        if (config.socketPath !== undefined) {
            await clearStaleSocket(config.socketPath);
            servers.push(Bun.serve({ unix: config.socketPath, fetch }));
            // Bun binds with the umask (0755 under 022, which no one else can
            // connect() to), so the mode is set rather than left to chance. The
            // group is the process's primary group: membership is the access check.
            await chmod(config.socketPath, 0o660);
            urls.push(`unix:${config.socketPath}`);
        }
        if (config.port !== undefined) {
            const server = Bun.serve({ hostname: config.hostname, port: config.port, fetch });
            servers.push(server);
            urls.push(`http://${server.hostname}:${server.port}`);
        }
    } catch (error) {
        await stop();
        throw error;
    }
    return { urls, stop };
}

/**
 * A socket left behind (a crash, a SIGKILL, or the directory kept across a
 * restart by systemd's RuntimeDirectoryPreserve) makes bind fail with
 * EADDRINUSE, so it is removed first — but only if nobody is answering on it.
 * A live one is another broker, very likely on the same store, which the
 * single-writer design does not allow. Anything that is not a socket is
 * someone's file, and is never deleted.
 */
async function clearStaleSocket(path: string): Promise<void> {
    let stats;
    try {
        stats = await lstat(path);
    } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
        throw error;
    }
    if (!stats.isSocket()) throw new Error(`${path} exists and is not a socket; refusing to replace it`);
    if (await answering(path)) throw new Error(`${path} is already being served by another process`);
    await unlink(path);
}

async function answering(path: string): Promise<boolean> {
    try {
        const socket = await Bun.connect({ unix: path, socket: { data() {} } });
        socket.end();
        return true;
    } catch {
        return false;
    }
}

/** How `serve` tells a supervisor it is listening. */
export type Notify = (env: Record<string, string | undefined>) => Promise<void>;

/**
 * Readiness for systemd `Type=notify`. Bun has no sd_notify and cannot send to
 * a unix datagram socket, so `systemd-notify --ready` is spawned; it waits for
 * systemd to process the message, and the unit needs `NotifyAccess=all` since
 * the sender is a child. Without NOTIFY_SOCKET (Docker, a shell) it does nothing.
 */
export const systemdNotify: Notify = async (env) => {
    if (!env.NOTIFY_SOCKET) return;
    const child = Bun.spawn(["systemd-notify", "--ready"], { env, stdout: "inherit", stderr: "inherit" });
    const code = await child.exited;
    if (code !== 0) throw new Error(`systemd-notify --ready exited ${code}`);
};

/**
 * Listen, then say so. `container().app` is the Fetch Bun serves. Ready is
 * signalled only once every listener is bound, so a client ordered after the
 * service never finds the socket missing.
 */
export async function start(
    c: Pick<Container, "config" | "app" | "logger">,
    notify: Notify = systemdNotify,
    env: Record<string, string | undefined> = process.env,
): Promise<Listening> {
    const listening = await listen(c.config, c.app);
    try {
        await notify(env);
    } catch (error) {
        await listening.stop();
        throw error;
    }
    c.logger.log(`treesinger ${version} serving on ${listening.urls.join(" and ")}`);
    return listening;
}

/**
 * Run the broker. Returns a promise that never resolves: the process stays
 * alive serving until it is signalled, and then closes its listeners (which
 * removes the socket) before exiting.
 */
export async function serve(c: Container): Promise<number> {
    const listening = await start(c);
    for (const signal of ["SIGTERM", "SIGINT"] as const) {
        process.once(signal, () => void listening.stop().finally(() => process.exit(0)));
    }
    return new Promise<number>(() => {});
}

/**
 * Probe a running broker's `/health` — the container HEALTHCHECK, self-contained
 * so the slim runtime image needs no curl/wget. Over the socket when one is
 * configured, else the TCP port on loopback (or the one address it binds).
 */
export async function health(c: Container): Promise<number> {
    const config: Listeners = c.config;
    try {
        const res = await fetch(healthUrl(config), config.socketPath ? { unix: config.socketPath } : {});
        return res.ok ? 0 : 1;
    } catch {
        return 1;
    }
}

function healthUrl(config: Listeners): string {
    // Any host will do over a socket; it only fills in the URL.
    if (config.socketPath !== undefined) return "http://treesinger/health";
    const wildcard = ["0.0.0.0", "::", ""].includes(config.hostname);
    const host = wildcard ? "127.0.0.1" : config.hostname.includes(":") ? `[${config.hostname}]` : config.hostname;
    return `http://${host}:${config.port}/health`;
}
