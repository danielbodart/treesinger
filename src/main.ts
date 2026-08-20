#!/usr/bin/env bun
import { container, type Container } from "./container.ts";

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
        default:
            console.error("usage: treesinger <login|serve>");
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

/** Run the broker. `container().app` is the Fetch Bun serves. */
function serve(c: Container): number {
    const server = Bun.serve({ port: c.config.port, fetch: c.app });
    console.log(`treesinger serving on http://${server.hostname}:${server.port}`);
    return 0;
}

process.exit(await main(Bun.argv));
