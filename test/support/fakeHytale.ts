import type { Fetch } from "../../src/http.ts";

/**
 * In-memory stand-in for Hypixel's services: a `Fetch` that replays recorded
 * response shapes from a route table and logs every call, so tests can both
 * script upstream behaviour (pending → success, rotation, errors) and assert on
 * how many times a route was hit (e.g. exactly one refresh under concurrency).
 *
 * `/oauth2/token` serves three grants, so it is keyed by `grant_type`; every
 * other endpoint is keyed by `METHOD /pathname`.
 */
export const ROUTES = {
    deviceAuth: "POST /oauth2/device/auth",
    deviceToken: "POST /oauth2/token urn:ietf:params:oauth:grant-type:device_code",
    refresh: "POST /oauth2/token refresh_token",
    profiles: "GET /my-account/get-profiles",
    newSession: "POST /game-session/new",
    refreshSession: "POST /game-session/refresh",
    terminate: "DELETE /game-session",
} as const;

export interface ScriptedResponse {
    status?: number;
    json: unknown;
}

interface Call {
    method: string;
    pathname: string;
    body: string;
    authorization: string | null;
}

export class FakeHytale {
    readonly calls: Call[] = [];
    private readonly scripts = new Map<string, ScriptedResponse[]>();

    /**
     * Script the responses for a route. Multiple responses are consumed in order;
     * the last one repeats for any further calls (so a steady state is one entry).
     */
    on(route: string, ...responses: ScriptedResponse[]): this {
        this.scripts.set(route, responses);
        return this;
    }

    readonly fetch: Fetch = async (request) => {
        const url = new URL(request.url);
        const body = request.method === "GET" || request.method === "DELETE" ? "" : await request.text();
        const route = keyFor(request.method, url.pathname, body);
        this.calls.push({
            method: request.method,
            pathname: url.pathname,
            body,
            authorization: request.headers.get("authorization"),
        });
        const queue = this.scripts.get(route);
        if (!queue || queue.length === 0) throw new Error(`FakeHytale: no response scripted for '${route}'`);
        const next = queue.length > 1 ? queue.shift()! : queue[0]!;
        return new Response(JSON.stringify(next.json), {
            status: next.status ?? 200,
            headers: { "content-type": "application/json" },
        });
    };

    /** How many times a given route was called. */
    countOf(route: string): number {
        return this.calls.filter((c) => keyFor(c.method, c.pathname, c.body) === route).length;
    }
}

function keyFor(method: string, pathname: string, body: string): string {
    if (pathname === "/oauth2/token") {
        return `POST /oauth2/token ${new URLSearchParams(body).get("grant_type") ?? ""}`;
    }
    return `${method} ${pathname}`;
}
