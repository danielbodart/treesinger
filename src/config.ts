import { homedir } from "node:os";
import { join } from "node:path";

/**
 * Static runtime configuration, sourced from the environment. All values have
 * sensible defaults except the refresh token, which the `login` command seeds
 * into the store separately.
 */
export interface Config {
    /** Where the token store lives on the mounted volume. */
    storePath: string;
    /** Refresh the OAuth access token when fewer than this many ms of life remain. */
    skewMs: number;
    /** TCP port `serve` listens on, or undefined for no TCP listener. */
    port?: number;
    /** Address the TCP listener binds. */
    hostname: string;
    /**
     * Unix socket `serve` listens on, or undefined for none. Created by the
     * broker itself (Bun cannot serve on a socket inherited from systemd), mode
     * 0660 with the process's primary group — that group is the access boundary.
     */
    socketPath?: string;
    /**
     * Preferred profile to mint sessions for when the account has more than one.
     * Unset → the first profile from `get-profiles`. A request body may override
     * per-call.
     */
    ownerUuid?: string;
    /**
     * Optional shared secret for `/v1/session`. Off by default — the boundary is
     * Docker network isolation, or the socket's group; this is opt-in hardening.
     */
    bearerToken?: string;
}

const configDir = process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config");

/**
 * TCP stays on 8080 unless a socket is configured, so the Docker image (which
 * sets neither) is unchanged; a socket alone means no TCP at all, and setting
 * both listens on both.
 */
export function configFromEnv(env: Record<string, string | undefined> = process.env): Config {
    const socketPath = env.TREESINGER_SOCKET || undefined;
    return {
        storePath: env.TREESINGER_STORE ?? join(configDir, "treesinger", "tokens.json"),
        skewMs: env.TREESINGER_SKEW_MS ? Number(env.TREESINGER_SKEW_MS) : 5 * 60_000,
        port: env.TREESINGER_PORT ? Number(env.TREESINGER_PORT) : socketPath ? undefined : 8080,
        hostname: env.TREESINGER_HOST || "0.0.0.0",
        socketPath,
        ownerUuid: env.TREESINGER_OWNER_UUID,
        bearerToken: env.TREESINGER_BEARER_TOKEN,
    };
}
