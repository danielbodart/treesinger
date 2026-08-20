import { constructor, LazyMap } from "@bodar/yadic/LazyMap.ts";
import { SystemClock } from "./clock.ts";
import { type Fetch, loggingFetch, retryingFetch } from "./http.ts";
import { FileTokenStore } from "./store.ts";
import { OAuthClient } from "./oauth.ts";
import { SessionClient } from "./sessions.ts";
import { bearerGate, handler } from "./app.ts";
import { configFromEnv } from "./config.ts";

/**
 * The one container factory. Production calls it once; each test calls it fresh
 * for an isolated graph. Leaves (`config`, `clock`, `logger`, `sleep`, `store`,
 * `fetch`) carry production defaults but stay settable until first access — so a
 * test overwrites them with doubles *before* realising the graph (see README /
 * the test files). Everything downstream is derived, never overridden.
 */
export function container() {
    return LazyMap.create()
        .set("config", () => configFromEnv())
        .set("clock", constructor(SystemClock))
        .set("logger", () => console)
        .set("sleep", () => (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)))
        .set("store", constructor(FileTokenStore))
        .set("fetch", () => globalThis.fetch.bind(globalThis) as Fetch)
        // Same-shape wrappers: retry transient upstream failures, then log.
        .decorate("fetch", ({ fetch, logger }) => loggingFetch(logger, retryingFetch(fetch)))
        .set("oauth", constructor(OAuthClient))
        .set("sessions", constructor(SessionClient))
        .set("app", (deps) => handler(deps))
        // Optional hardening: gate /v1/session behind a bearer token when configured.
        .decorate("app", ({ app, config }) => (config.bearerToken ? bearerGate(config.bearerToken, app) : app));
}

export type Container = ReturnType<typeof container>;
