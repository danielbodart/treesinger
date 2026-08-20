import { type Fetch, type Logger, UpstreamError } from "./http.ts";
import type { SessionClient } from "./sessions.ts";
import type { OAuthClient } from "./oauth.ts";

export interface AppDeps {
    sessions: SessionClient;
    oauth: OAuthClient;
    logger: Logger;
}

/**
 * Treesinger's own HTTP surface, as a `Fetch`.
 *
 * - `POST /v1/session` — mint a fresh pair for the operator profile (optional
 *   `{ "uuid": "..." }` body overrides which profile). What a booting server calls.
 * - `GET /health` — 200 only when a mint would succeed (refresh token present AND
 *   a valid access token obtainable). Backs compose `service_healthy`.
 */
export function handler(deps: AppDeps): Fetch {
    return async (request) => {
        const { pathname } = new URL(request.url);

        if (request.method === "POST" && pathname === "/v1/session") {
            return mintSession(deps, request);
        }
        if (request.method === "GET" && pathname === "/health") {
            return health(deps);
        }
        return json({ error: "not_found" }, 404);
    };
}

async function mintSession(deps: AppDeps, request: Request): Promise<Response> {
    try {
        const uuid = await profileFromBody(request);
        const session = await deps.sessions.newSession(uuid);
        return json(session, 200);
    } catch (error) {
        return errorResponse(deps.logger, error);
    }
}

async function health(deps: AppDeps): Promise<Response> {
    try {
        await deps.oauth.ensureAccessToken();
        return json({ status: "ok" }, 200);
    } catch {
        return json({ status: "unauthenticated" }, 503);
    }
}

/** Read an optional `{ uuid }` from the body; tolerate an empty body. */
async function profileFromBody(request: Request): Promise<string | undefined> {
    const text = await request.text();
    if (!text) return undefined;
    const body = JSON.parse(text) as { uuid?: string };
    return body.uuid;
}

/** Map an upstream failure back to a client status; anything else is a 500. */
function errorResponse(logger: Logger, error: unknown): Response {
    if (error instanceof UpstreamError) {
        return json({ error: error.code }, mapStatus(error.status));
    }
    logger.error("mint failed", error);
    return json({ error: "internal" }, 500);
}

function mapStatus(upstream: number): number {
    if (upstream === 401) return 401;
    if (upstream === 403) return 403;
    if (upstream === 404) return 404;
    return 502; // any other upstream failure is a bad-gateway from the fleet's view
}

function json(body: unknown, status: number): Response {
    return new Response(JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json" },
    });
}

/**
 * Optional bearer-token gate for `/v1/session`. Off unless a token is configured
 * — the primary boundary is network isolation. `/health` stays open so compose
 * health checks need no secret.
 */
export function bearerGate(token: string, inner: Fetch): Fetch {
    return async (request) => {
        const { pathname } = new URL(request.url);
        if (pathname === "/v1/session" && request.headers.get("authorization") !== `Bearer ${token}`) {
            return json({ error: "unauthorized" }, 401);
        }
        return inner(request);
    };
}
