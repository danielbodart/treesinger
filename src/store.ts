import { mkdir, rename, writeFile, readFile, chmod } from "node:fs/promises";
import { dirname } from "node:path";

/**
 * The persisted credential set. `refresh_token` is the one long-lived secret —
 * it rotates on every OAuth refresh (old one invalidated), so it MUST be written
 * back before any refreshed token is handed out. `access_token` is a cache;
 * `access_expires_at` is epoch milliseconds.
 */
export interface StoredTokens {
    refresh_token: string;
    access_token?: string;
    access_expires_at?: number;
}

export interface TokenStore {
    /** The stored tokens, or `undefined` when no one has logged in yet. */
    read(): Promise<StoredTokens | undefined>;
    /** Persist tokens durably. Implementations must be crash-atomic. */
    write(tokens: StoredTokens): Promise<void>;
}

/**
 * File-on-volume store. Mirrors the proven chandlery `tools/hytale-token`
 * semantics: the credential file (and its directory) are owner-only, and writes
 * are atomic — a temp file `chmod 600` then `rename`d over the target — so a
 * crash after a refresh can never strand a spent refresh token as the stored one
 * or briefly expose the file world-readable.
 *
 * There is no OS-level lock here (unlike the multi-process CLI it descends from):
 * Treesinger is the sole, single-process writer, and refresh is serialised in
 * memory (see OAuthClient's single-flight). Running multiple writer instances
 * against one store would reintroduce the race and is out of scope by design.
 */
export class FileTokenStore implements TokenStore {
    private readonly path: string;

    constructor(deps: { config: { storePath: string } }) {
        this.path = deps.config.storePath;
    }

    async read(): Promise<StoredTokens | undefined> {
        try {
            return JSON.parse(await readFile(this.path, "utf8")) as StoredTokens;
        } catch (error) {
            if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
            throw error;
        }
    }

    async write(tokens: StoredTokens): Promise<void> {
        await mkdir(dirname(this.path), { recursive: true, mode: 0o700 });
        const tmp = `${this.path}.${process.pid}.tmp`;
        await writeFile(tmp, JSON.stringify(tokens), { mode: 0o600 });
        await chmod(tmp, 0o600); // writeFile honours umask; force owner-only.
        await rename(tmp, this.path);
    }
}

/** In-memory store for tests. Same contract, no disk. */
export class MemoryTokenStore implements TokenStore {
    private tokens?: StoredTokens;

    constructor(seed?: StoredTokens) {
        this.tokens = seed;
    }

    async read(): Promise<StoredTokens | undefined> {
        return this.tokens ? { ...this.tokens } : undefined;
    }

    async write(tokens: StoredTokens): Promise<void> {
        this.tokens = { ...tokens };
    }
}
