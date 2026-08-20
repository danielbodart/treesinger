import type { Clock } from "./clock.ts";
import { type Fetch, type Logger, ok, safeJson, UpstreamError } from "./http.ts";
import type { StoredTokens, TokenStore } from "./store.ts";
import type { Config } from "./config.ts";

/** OAuth 2.0 client for `client_id=hytale-server`, per the Server Provider guide. */
const CLIENT_ID = "hytale-server";
const SCOPE = "openid offline auth:server";
const DEVICE_AUTH_URL = "https://oauth.accounts.hytale.com/oauth2/device/auth";
const TOKEN_URL = "https://oauth.accounts.hytale.com/oauth2/token";
const DEVICE_CODE_GRANT = "urn:ietf:params:oauth:grant-type:device_code";

export interface DeviceAuth {
    device_code: string;
    user_code: string;
    verification_uri: string;
    verification_uri_complete: string;
    expires_in: number;
    interval: number;
}

interface TokenResponse {
    access_token: string;
    token_type: string;
    expires_in: number;
    refresh_token: string;
    scope: string;
}

export interface OAuthDeps {
    fetch: Fetch;
    clock: Clock;
    store: TokenStore;
    logger: Logger;
    config: Config;
    /** Between-poll wait during the device flow. Instant in tests. */
    sleep: (ms: number) => Promise<void>;
}

export class OAuthClient {
    private readonly fetch: Fetch;
    private readonly clock: Clock;
    private readonly store: TokenStore;
    private readonly logger: Logger;
    private readonly config: Config;
    private readonly sleep: (ms: number) => Promise<void>;

    /**
     * The single-flight latch: concurrent `ensureAccessToken` callers that all
     * find the cached token stale share ONE refresh instead of each spending the
     * rotating refresh token. This — not a lock — is what makes double-spend
     * structurally impossible.
     */
    private inflight?: Promise<string>;

    constructor(deps: OAuthDeps) {
        this.fetch = deps.fetch;
        this.clock = deps.clock;
        this.store = deps.store;
        this.logger = deps.logger;
        this.config = deps.config;
        this.sleep = deps.sleep;
    }

    /** §1 — start the device flow. Show the user `verification_uri` + `user_code`. */
    async deviceAuth(): Promise<DeviceAuth> {
        const response = await this.fetch(form(DEVICE_AUTH_URL, { client_id: CLIENT_ID, scope: SCOPE }));
        return (await ok(response)) as DeviceAuth;
    }

    /**
     * §2 — poll the token endpoint until the user approves, then persist the
     * resulting tokens (this is what `login` writes). `authorization_pending` and
     * `slow_down` continue the poll; any other error aborts.
     */
    async pollToken(deviceCode: string, intervalSec: number): Promise<StoredTokens> {
        let interval = intervalSec;
        for (;;) {
            await this.sleep(interval * 1000);
            const response = await this.fetch(form(TOKEN_URL, {
                client_id: CLIENT_ID,
                grant_type: DEVICE_CODE_GRANT,
                device_code: deviceCode,
            }));
            if (response.ok) {
                const token = (await response.json()) as TokenResponse;
                return this.persist(token);
            }
            const body = await safeJson(response);
            const error = body?.error;
            if (error === "authorization_pending") continue;
            if (error === "slow_down") {
                interval += 5;
                continue;
            }
            throw new UpstreamError(response.status, error ?? "unknown", `device token poll failed: ${error ?? response.status}`);
        }
    }

    /**
     * A valid OAuth access token, refreshing on demand. Fast path: a cached token
     * with more than the skew window of life left is returned without any network
     * call. Otherwise a single shared refresh runs (see `inflight`).
     */
    async ensureAccessToken(): Promise<string> {
        const stored = await this.store.read();
        if (!stored) throw new UpstreamError(401, "not_logged_in", "no refresh token in store — run `treesinger login`");
        if (stored.access_token && this.isFresh(stored)) return stored.access_token;
        return (this.inflight ??= this.refresh().finally(() => {
            this.inflight = undefined;
        }));
    }

    /**
     * §6 — spend the refresh token for a fresh access+refresh pair. The rotated
     * refresh token is written to the store BEFORE the new access token is
     * returned, so a crash mid-flight can never lose the only live credential.
     * Returns the new access token.
     */
    private async refresh(): Promise<string> {
        const stored = await this.store.read();
        if (!stored?.refresh_token) throw new UpstreamError(401, "not_logged_in", "no refresh token in store");
        // Double-check under the single-flight: a refresh that completed between
        // the caller's cache read and here has already rotated + stored a fresh
        // token — return it rather than spending the (now-invalidated) old one.
        if (stored.access_token && this.isFresh(stored)) return stored.access_token;
        this.logger.log("refreshing OAuth access token");
        const response = await this.fetch(form(TOKEN_URL, {
            client_id: CLIENT_ID,
            grant_type: "refresh_token",
            refresh_token: stored.refresh_token,
        }));
        if (response.status === 401) throw new UpstreamError(401, "invalid_grant", "refresh token rejected — re-auth needed");
        const token = (await ok(response)) as TokenResponse;
        // Some servers omit a rotated token; keep the existing one if so.
        const persisted = await this.persist({ ...token, refresh_token: token.refresh_token || stored.refresh_token });
        return persisted.access_token!;
    }

    /** Compute expiry from `expires_in` and write the store atomically. */
    private async persist(token: TokenResponse): Promise<StoredTokens> {
        const tokens: StoredTokens = {
            refresh_token: token.refresh_token,
            access_token: token.access_token,
            access_expires_at: this.clock.now().getTime() + token.expires_in * 1000,
        };
        await this.store.write(tokens);
        return tokens;
    }

    /** Pure over clock + stored expiry: does the cached token clear the skew window? */
    private isFresh(stored: StoredTokens): boolean {
        return stored.access_expires_at != null
            && this.clock.now().getTime() < stored.access_expires_at - this.config.skewMs;
    }
}

/** Build a form-encoded POST Request. */
function form(url: string, fields: Record<string, string>): Request {
    return new Request(url, {
        method: "POST",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams(fields).toString(),
    });
}
