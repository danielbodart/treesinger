import { afterEach, describe, expect, test } from "bun:test";
import { stat, rm, readdir } from "node:fs/promises";
import { join } from "node:path";
import { FileTokenStore, MemoryTokenStore } from "../src/store.ts";

const scratch = join(process.env.TMPDIR ?? "/tmp", `treesinger-store-test-${process.pid}`);

afterEach(async () => {
    await rm(scratch, { recursive: true, force: true });
});

describe("FileTokenStore", () => {
    const storeAt = () => new FileTokenStore({ config: { storePath: join(scratch, "nested", "tokens.json") } });

    test("read is undefined before any write", async () => {
        expect(await storeAt().read()).toBeUndefined();
    });

    test("round-trips tokens, owner-only, with no temp file left behind", async () => {
        const store = storeAt();
        await store.write({ refresh_token: "rt", access_token: "at", access_expires_at: 123 });

        expect(await store.read()).toEqual({ refresh_token: "rt", access_token: "at", access_expires_at: 123 });

        const dir = join(scratch, "nested");
        expect((await stat(join(dir, "tokens.json"))).mode & 0o777).toBe(0o600);
        expect((await stat(dir)).mode & 0o777).toBe(0o700);
        expect(await readdir(dir)).toEqual(["tokens.json"]); // atomic rename left no *.tmp
    });

    test("a second write replaces the first", async () => {
        const store = storeAt();
        await store.write({ refresh_token: "rt1" });
        await store.write({ refresh_token: "rt2" });
        expect((await store.read())?.refresh_token).toBe("rt2");
    });
});

describe("MemoryTokenStore", () => {
    test("round-trips and returns copies, not the held reference", async () => {
        const store = new MemoryTokenStore();
        const written = { refresh_token: "rt" };
        await store.write(written);
        const read = await store.read();
        expect(read).toEqual(written);
        expect(read).not.toBe(written);
    });
});
