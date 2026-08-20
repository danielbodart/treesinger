import { describe, expect, test } from "bun:test";
import { harness } from "./support/harness.ts";
import { ROUTES } from "./support/fakeHytale.ts";
import profiles from "./fixtures/profiles.json";
import gameSession from "./fixtures/game-session.json";
import sessionLimit from "./fixtures/error-session-limit.json";

/** A store whose cached access token is valid all through the tested window. */
const validSeed = {
    refresh_token: "rt",
    access_token: "access-token-1",
    access_expires_at: Date.parse("2026-08-20T02:00:00Z"),
};

describe("mint", () => {
    test("profiles → new session → the pair, minted for the first profile by default", async () => {
        const { c, fake } = harness({ seed: validSeed });
        fake.on(ROUTES.profiles, { json: profiles }).on(ROUTES.newSession, { json: gameSession });

        const session = await c.sessions.newSession();

        expect(session).toEqual(gameSession);
        const mintCall = fake.calls.find((call) => call.pathname === "/game-session/new")!;
        expect(JSON.parse(mintCall.body)).toEqual({ uuid: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" });
        expect(mintCall.authorization).toBe("Bearer access-token-1");
    });

    test("configured ownerUuid wins and skips the profiles lookup", async () => {
        const { c, fake } = harness({ seed: validSeed, config: { ownerUuid: "dddddddd-dddd-dddd-dddd-dddddddddddd" } });
        fake.on(ROUTES.newSession, { json: gameSession });

        await c.sessions.newSession();

        expect(fake.countOf(ROUTES.profiles)).toBe(0);
        const mintCall = fake.calls.find((call) => call.pathname === "/game-session/new")!;
        expect(JSON.parse(mintCall.body)).toEqual({ uuid: "dddddddd-dddd-dddd-dddd-dddddddddddd" });
    });

    test("a per-call uuid override beats both config and the default", async () => {
        const { c, fake } = harness({ seed: validSeed, config: { ownerUuid: "dddddddd-dddd-dddd-dddd-dddddddddddd" } });
        fake.on(ROUTES.newSession, { json: gameSession });

        await c.sessions.newSession("cccccccc-cccc-cccc-cccc-cccccccccccc");

        const mintCall = fake.calls.find((call) => call.pathname === "/game-session/new")!;
        expect(JSON.parse(mintCall.body)).toEqual({ uuid: "cccccccc-cccc-cccc-cccc-cccccccccccc" });
    });

    test("403 over the session limit surfaces as 403", async () => {
        const { c, fake } = harness({ seed: validSeed, config: { ownerUuid: "x" } });
        fake.on(ROUTES.newSession, { status: 403, json: sessionLimit });
        await expect(c.sessions.newSession()).rejects.toMatchObject({ status: 403, code: "session_limit_exceeded" });
    });
});

describe("profiles cache", () => {
    test("cached within the TTL, refetched after it", async () => {
        const { c, fake, clock } = harness({ seed: validSeed });
        fake.on(ROUTES.profiles, { json: profiles });

        await c.sessions.getProfiles();
        await c.sessions.getProfiles();
        expect(fake.countOf(ROUTES.profiles)).toBe(1); // second served from cache

        clock.advance(61 * 60_000); // past the 1h TTL
        await c.sessions.getProfiles();
        expect(fake.countOf(ROUTES.profiles)).toBe(2);
    });
});
