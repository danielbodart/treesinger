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
    /** Port `serve` listens on. */
    port: number;
    /**
     * Preferred profile to mint sessions for when the account has more than one.
     * Unset → the first profile from `get-profiles`. A request body may override
     * per-call.
     */
    ownerUuid?: string;
    /**
     * Optional shared secret for `/v1/session`. Off by default — the boundary is
     * Docker network isolation; this is opt-in hardening.
     */
    bearerToken?: string;
}

const configDir = process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config");

export function configFromEnv(env: Record<string, string | undefined> = process.env): Config {
    return {
        storePath: env.TREESINGER_STORE ?? join(configDir, "treesinger", "tokens.json"),
        skewMs: env.TREESINGER_SKEW_MS ? Number(env.TREESINGER_SKEW_MS) : 5 * 60_000,
        port: env.TREESINGER_PORT ? Number(env.TREESINGER_PORT) : 8080,
        ownerUuid: env.TREESINGER_OWNER_UUID,
        bearerToken: env.TREESINGER_BEARER_TOKEN,
    };
}
