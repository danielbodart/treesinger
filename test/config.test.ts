import { describe, expect, test } from "bun:test";
import { configFromEnv } from "../src/config.ts";

describe("configFromEnv listeners", () => {
    test("nothing set is TCP 8080 on every address, as the image has always been", () => {
        const config = configFromEnv({});
        expect(config.port).toBe(8080);
        expect(config.hostname).toBe("0.0.0.0");
        expect(config.socketPath).toBeUndefined();
    });

    test("a socket alone means no TCP listener", () => {
        const config = configFromEnv({ TREESINGER_SOCKET: "/run/treesinger/treesinger.sock" });
        expect(config.socketPath).toBe("/run/treesinger/treesinger.sock");
        expect(config.port).toBeUndefined();
    });

    test("a socket and a port listen on both", () => {
        const config = configFromEnv({ TREESINGER_SOCKET: "/run/t.sock", TREESINGER_PORT: "9090" });
        expect(config.socketPath).toBe("/run/t.sock");
        expect(config.port).toBe(9090);
    });

    test("the TCP bind address is honoured", () => {
        expect(configFromEnv({ TREESINGER_HOST: "127.0.0.1" }).hostname).toBe("127.0.0.1");
    });

    test("empty values count as unset", () => {
        const config = configFromEnv({ TREESINGER_SOCKET: "", TREESINGER_HOST: "" });
        expect(config.socketPath).toBeUndefined();
        expect(config.port).toBe(8080);
        expect(config.hostname).toBe("0.0.0.0");
    });
});
