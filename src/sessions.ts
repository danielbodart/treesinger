import type { Clock } from "./clock.ts";
import { type Fetch, UpstreamError } from "./http.ts";
import type { OAuthClient } from "./oauth.ts";
import type { Config } from "./config.ts";

const PROFILES_URL = "https://account-data.hytale.com/my-account/get-profiles";
const SESSION_NEW_URL = "https://sessions.hytale.com/game-session/new";
const SESSION_REFRESH_URL = "https://sessions.hytale.com/game-session/refresh";
const SESSION_URL = "https://sessions.hytale.com/game-session";

/** Cache profiles for an hour; they change rarely and each lookup costs a call. */
const PROFILES_TTL_MS = 60 * 60_000;

export interface Profile {
    uuid: string;
    username: string;
}

export interface ProfilesResponse {
    owner: string;
    profiles: Profile[];
}

/** The pair a Hytale server runs with. `expiresAt` is an ISO-8601 instant. */
export interface GameSession {
    sessionToken: string;
    identityToken: string;
    expiresAt: string;
}

export interface SessionDeps {
    fetch: Fetch;
    clock: Clock;
    oauth: OAuthClient;
    config: Config;
}

export class SessionClient {
    private readonly fetch: Fetch;
    private readonly clock: Clock;
    private readonly oauth: OAuthClient;
    private readonly config: Config;

    private profilesCache?: { value: ProfilesResponse; at: number };

    constructor(deps: SessionDeps) {
        this.fetch = deps.fetch;
        this.clock = deps.clock;
        this.oauth = deps.oauth;
        this.config = deps.config;
    }

    /** §3 — the operator's account profiles, cached with a TTL over the clock. */
    async getProfiles(): Promise<ProfilesResponse> {
        const now = this.clock.now().getTime();
        if (this.profilesCache && now - this.profilesCache.at < PROFILES_TTL_MS) {
            return this.profilesCache.value;
        }
        const token = await this.oauth.ensureAccessToken();
        const response = await this.fetch(new Request(PROFILES_URL, { headers: bearer(token) }));
        const value = (await ok(response)) as ProfilesResponse;
        this.profilesCache = { value, at: now };
        return value;
    }

    /**
     * Which profile a mint targets: an explicit override, else the configured
     * `ownerUuid`, else the account's first profile.
     */
    async resolveProfile(override?: string): Promise<string> {
        if (override) return override;
        if (this.config.ownerUuid) return this.config.ownerUuid;
        const { profiles } = await this.getProfiles();
        const first = profiles[0];
        if (!first) throw new UpstreamError(404, "no_profile", "account has no profiles");
        return first.uuid;
    }

    /** §4 — mint a fresh session/identity pair for a profile. */
    async newSession(profileUuid?: string): Promise<GameSession> {
        const uuid = await this.resolveProfile(profileUuid);
        const token = await this.oauth.ensureAccessToken();
        const response = await this.fetch(new Request(SESSION_NEW_URL, {
            method: "POST",
            headers: { ...bearer(token), "content-type": "application/json" },
            body: JSON.stringify({ uuid }),
        }));
        return (await ok(response)) as GameSession;
    }

    /**
     * §5 — refresh a session with its own session token. Treesinger does not do
     * this for running servers (they self-refresh); exposed for completeness of
     * the client surface.
     */
    async refreshSession(sessionToken: string): Promise<GameSession> {
        const response = await this.fetch(new Request(SESSION_REFRESH_URL, {
            method: "POST",
            headers: bearer(sessionToken),
        }));
        return (await ok(response)) as GameSession;
    }

    /** Terminate a session. */
    async terminateSession(sessionToken: string): Promise<void> {
        const response = await this.fetch(new Request(SESSION_URL, {
            method: "DELETE",
            headers: bearer(sessionToken),
        }));
        if (!response.ok) {
            throw new UpstreamError(response.status, "terminate_failed", `terminate failed: ${response.status}`);
        }
    }
}

function bearer(token: string): Record<string, string> {
    return { authorization: `Bearer ${token}` };
}

async function ok(response: Response): Promise<unknown> {
    if (response.ok) return response.json();
    let code = "unknown";
    try {
        code = ((await response.json()) as { error?: string }).error ?? "unknown";
    } catch {
        // no JSON body
    }
    throw new UpstreamError(response.status, code, `upstream ${response.status}: ${code}`);
}
