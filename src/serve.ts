import type { Container } from "./container.ts";
import { version } from "./version.ts";

/**
 * Run the broker. `container().app` is the Fetch Bun serves. Returns a promise
 * that never resolves: the process stays alive serving until it is signalled,
 * rather than falling through to `process.exit`.
 */
export function serve(c: Container): Promise<number> {
    const server = Bun.serve({ port: c.config.port, fetch: c.app });
    console.log(`treesinger ${version} serving on http://${server.hostname}:${server.port}`);
    return new Promise<number>(() => {});
}

/**
 * Probe a running broker's `/health` on localhost — the container HEALTHCHECK,
 * self-contained so the slim runtime image needs no curl/wget.
 */
export async function health(c: Container): Promise<number> {
    try {
        const res = await fetch(`http://127.0.0.1:${c.config.port}/health`);
        return res.ok ? 0 : 1;
    } catch {
        return 1;
    }
}
