import { describe, expect, test } from "bun:test";
import { harness, validSeed } from "./support/harness.ts";
import { ROUTES } from "./support/fakeHytale.ts";
import profiles from "./fixtures/profiles.json";
import gameSession from "./fixtures/game-session.json";
import sessionLimit from "./fixtures/error-session-limit.json";

const post = (body?: unknown, headers?: Record<string, string>) =>
    new Request("http://broker/v1/session", {
        method: "POST",
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
    });

describe("POST /v1/session", () => {
    test("mints and returns the pair", async () => {
        const { c, fake } = harness({ seed: validSeed });
        fake.on(ROUTES.profiles, { json: profiles }).on(ROUTES.newSession, { json: gameSession });

        const res = await c.app(post());

        expect(res.status).toBe(200);
        expect(await res.json()).toEqual(gameSession);
    });

    test("honours a uuid in the body", async () => {
        const { c, fake } = harness({ seed: validSeed });
        fake.on(ROUTES.newSession, { json: gameSession });

        await c.app(post({ uuid: "cccccccc-cccc-cccc-cccc-cccccccccccc" }));

        const mintCall = fake.calls.find((call) => call.pathname === "/game-session/new")!;
        expect(JSON.parse(mintCall.body)).toEqual({ uuid: "cccccccc-cccc-cccc-cccc-cccccccccccc" });
    });

    test("a malformed body is a 400, not a 500", async () => {
        const { c } = harness({ seed: validSeed });
        const res = await c.app(new Request("http://broker/v1/session", { method: "POST", body: "{uuid:" }));
        expect(res.status).toBe(400);
        expect(await res.json()).toEqual({ error: "bad_request" });
    });

    test("a whitespace body falls back to the default profile", async () => {
        const { c, fake } = harness({ seed: validSeed, config: { ownerUuid: "x" } });
        fake.on(ROUTES.newSession, { json: gameSession });
        const res = await c.app(new Request("http://broker/v1/session", { method: "POST", body: "  " }));
        expect(res.status).toBe(200);
    });

    test("maps a 403 session-limit through to a 403", async () => {
        const { c, fake } = harness({ seed: validSeed, config: { ownerUuid: "x" } });
        fake.on(ROUTES.newSession, { status: 403, json: sessionLimit });

        const res = await c.app(post());

        expect(res.status).toBe(403);
        expect(await res.json()).toEqual({ error: "session_limit_exceeded" });
    });
});

describe("GET /health", () => {
    const get = () => new Request("http://broker/health");

    test("200 when a mint would succeed", async () => {
        const { c } = harness({ seed: validSeed });
        const res = await c.app(get());
        expect(res.status).toBe(200);
        expect(await res.json()).toEqual({ status: "ok" });
    });

    test("503 when there is no login yet", async () => {
        const { c } = harness();
        const res = await c.app(get());
        expect(res.status).toBe(503);
    });
});

test("unknown route is 404", async () => {
    const { c } = harness({ seed: validSeed });
    const res = await c.app(new Request("http://broker/nope"));
    expect(res.status).toBe(404);
});

describe("optional bearer gate", () => {
    test("gates /v1/session but leaves /health open", async () => {
        const { c, fake } = harness({ seed: validSeed, config: { bearerToken: "s3cret" } });
        fake.on(ROUTES.newSession, { json: gameSession });

        expect((await c.app(post({ uuid: "x" }))).status).toBe(401); // no header
        expect((await c.app(post({ uuid: "x" }, { authorization: "Bearer wrong" }))).status).toBe(401);
        expect((await c.app(new Request("http://broker/health"))).status).toBe(200); // health stays open

        const res = await c.app(post({ uuid: "x" }, { authorization: "Bearer s3cret" }));
        expect(res.status).toBe(200);
    });
});
