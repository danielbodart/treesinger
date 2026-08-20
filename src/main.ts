#!/usr/bin/env bun
import { container, type Container } from "./container.ts";
import { version } from "./version.ts";

/**
 * Two subcommands of one binary, sharing one container:
 *   treesinger login   — the one-time device login; writes the refresh token.
 *   treesinger serve    — run the mint-on-request broker.
 */
async function main(argv: string[]): Promise<number> {
    const command = argv[2];
    const c = container();
    switch (command) {
        case "login":
            return login(c);
        case "serve":
            return serve(c);
        case "health":
            return health(c);
        case "version":
        case "--version":
            console.log(version);
            return 0;
        default:
            console.error("usage: treesinger <login|serve|health|version>");
            return command ? 1 : 0;
    }
}

/** §1–2 — run the device flow, print the code, poll, persist the refresh token. */
async function login(c: Container): Promise<number> {
    const auth = await c.oauth.deviceAuth();
    console.log("\nTo authorise Treesinger, visit:\n");
    console.log(`  ${auth.verification_uri_complete}`);
    console.log(`\nor go to ${auth.verification_uri} and enter code: ${auth.user_code}\n`);
    console.log("Waiting for approval…");
    await c.oauth.pollToken(auth.device_code, auth.interval);
    console.log("✓ Logged in. Refresh token stored. You can now `treesinger serve`.");
    return 0;
}

/**
 * Run the broker. `container().app` is the Fetch Bun serves. Returns a promise
 * that never resolves: the process stays alive serving until it is signalled,
 * rather than falling through to `process.exit`.
 */
function serve(c: Container): Promise<number> {
    const server = Bun.serve({ port: c.config.port, fetch: c.app });
    console.log(`treesinger ${version} serving on http://${server.hostname}:${server.port}`);
    return new Promise<number>(() => {});
}

/**
 * Probe a running broker's `/health` on localhost — the container HEALTHCHECK,
 * self-contained so the slim runtime image needs no curl/wget.
 */
async function health(c: Container): Promise<number> {
    try {
        const res = await fetch(`http://127.0.0.1:${c.config.port}/health`);
        return res.ok ? 0 : 1;
    } catch {
        return 1;
    }
}

process.exit(await main(Bun.argv));
