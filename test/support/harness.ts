import { instance } from "@bodar/yadic/LazyMap.ts";
import { container } from "../../src/container.ts";
import { StoppedClock } from "../../src/clock.ts";
import { MemoryTokenStore, type StoredTokens, type TokenStore } from "../../src/store.ts";
import type { Config } from "../../src/config.ts";
import type { Logger } from "../../src/http.ts";
import { FakeHytale } from "./fakeHytale.ts";

/** Discards output; keeps test runs quiet while still satisfying `Logger`. */
export const silentLogger: Logger = { log() {}, warn() {}, error() {} };

/** A seed whose cached access token stays valid across a test's clock window. */
export const validSeed: StoredTokens = {
    refresh_token: "rt",
    access_token: "access-token-1",
    access_expires_at: Date.parse("2026-08-20T02:00:00Z"),
};

const baseConfig: Config = {
    storePath: "/unused-in-tests",
    skewMs: 5 * 60_000,
    port: 0,
};

export interface Harness {
    clock: StoppedClock;
    fake: FakeHytale;
    store: TokenStore;
    config: Config;
    /** The realised container — the first access here freezes the doubles in. */
    c: ReturnType<typeof container>;
}

/**
 * Build the production container and overwrite its leaf doubles BEFORE anything
 * reads them — the one legal mutation (a lazy prop is settable until first
 * access). This is why there is no test child container.
 */
export function harness(opts: {
    now?: string;
    seed?: StoredTokens;
    store?: TokenStore;
    config?: Partial<Config>;
} = {}): Harness {
    const clock = new StoppedClock(new Date(opts.now ?? "2026-08-20T00:00:00Z"));
    const fake = new FakeHytale();
    const store = opts.store ?? new MemoryTokenStore(opts.seed);
    const config: Config = { ...baseConfig, ...opts.config };

    const c = container();
    c.set("clock", instance(clock));
    c.set("fetch", instance(fake.fetch));
    c.set("store", instance(store));
    c.set("config", instance(config));
    c.set("logger", instance(silentLogger));
    c.set("sleep", instance(() => Promise.resolve())); // no real timers in tests

    return { clock, fake, store, config, c };
}
