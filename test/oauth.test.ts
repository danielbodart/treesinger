import { describe, expect, test } from "bun:test";
import { harness } from "./support/harness.ts";
import { ROUTES } from "./support/fakeHytale.ts";
import { UpstreamError } from "../src/http.ts";
import deviceAuth from "./fixtures/device-auth.json";
import token from "./fixtures/token.json";
import refreshed from "./fixtures/token-refreshed.json";
import pending from "./fixtures/authorization-pending.json";
import invalidGrant from "./fixtures/error-invalid-grant.json";

describe("device flow", () => {
    test("polls through pending, then stores the refresh token on success", async () => {
        const { c, fake, store } = harness();
        fake.on(ROUTES.deviceAuth, { json: deviceAuth })
            .on(ROUTES.deviceToken, { status: 400, json: pending }, { status: 400, json: pending }, { json: token });

        const auth = await c.oauth.deviceAuth();
        await c.oauth.pollToken(auth.device_code, auth.interval);

        expect((await store.read())?.refresh_token).toBe("refresh-token-1");
        expect(fake.countOf(ROUTES.deviceToken)).toBe(3); // two pending + one success
    });

    test("aborts on a hard error", async () => {
        const { c, fake } = harness();
        fake.on(ROUTES.deviceToken, { status: 400, json: { error: "access_denied" } });
        await expect(c.oauth.pollToken("dev-code", 5)).rejects.toBeInstanceOf(UpstreamError);
    });
});

describe("ensureAccessToken", () => {
    test("cache hit: a comfortably-valid access token makes no network call", async () => {
        const { c, fake } = harness({
            seed: { refresh_token: "rt", access_token: "cached", access_expires_at: Date.parse("2026-08-20T00:30:00Z") },
        });
        expect(await c.oauth.ensureAccessToken()).toBe("cached");
        expect(fake.calls).toHaveLength(0);
    });

    test("refreshes when inside the skew window, and stores the rotated token before returning", async () => {
        // Access token expires in 2 min; skew is 5 min → stale.
        const { c, fake, store } = harness({
            seed: { refresh_token: "refresh-token-1", access_token: "old", access_expires_at: Date.parse("2026-08-20T00:02:00Z") },
        });
        fake.on(ROUTES.refresh, { json: refreshed });

        const at = await c.oauth.ensureAccessToken();

        expect(at).toBe("access-token-2");
        const stored = await store.read();
        expect(stored?.refresh_token).toBe("refresh-token-2"); // rotated + persisted
        expect(stored?.access_token).toBe("access-token-2");
        expect(fake.countOf(ROUTES.refresh)).toBe(1);
    });

    test("the refresh-or-not decision is pure over the clock", async () => {
        const { c, fake, clock, store } = harness({
            seed: { refresh_token: "refresh-token-1", access_token: "old", access_expires_at: Date.parse("2026-08-20T01:00:00Z") },
        });
        fake.on(ROUTES.refresh, { json: refreshed });

        // 54 min in: 6 min of life left, clears the 5 min skew → cache hit.
        clock.advance(54 * 60_000);
        expect(await c.oauth.ensureAccessToken()).toBe("old");
        expect(fake.countOf(ROUTES.refresh)).toBe(0);

        // 2 more min: 4 min left, inside skew → refresh.
        clock.advance(2 * 60_000);
        expect(await c.oauth.ensureAccessToken()).toBe("access-token-2");
        expect(fake.countOf(ROUTES.refresh)).toBe(1);
        void store;
    });

    test("two concurrent stale calls share ONE refresh (no double-spend)", async () => {
        const { c, fake } = harness({
            seed: { refresh_token: "refresh-token-1", access_expires_at: Date.parse("2026-08-20T00:02:00Z") },
        });
        fake.on(ROUTES.refresh, { json: refreshed });

        const [a, b] = await Promise.all([c.oauth.ensureAccessToken(), c.oauth.ensureAccessToken()]);

        expect(a).toBe("access-token-2");
        expect(b).toBe("access-token-2");
        expect(fake.countOf(ROUTES.refresh)).toBe(1); // the rotating token spent exactly once
    });

    test("401 on refresh surfaces as re-auth-needed", async () => {
        const { c, fake } = harness({
            seed: { refresh_token: "stale", access_expires_at: Date.parse("2026-08-20T00:02:00Z") },
        });
        fake.on(ROUTES.refresh, { status: 401, json: invalidGrant });
        await expect(c.oauth.ensureAccessToken()).rejects.toMatchObject({ status: 401 });
    });

    test("no store at all is a 401, not a crash", async () => {
        const { c } = harness();
        await expect(c.oauth.ensureAccessToken()).rejects.toMatchObject({ status: 401, code: "not_logged_in" });
    });
});
