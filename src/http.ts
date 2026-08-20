/**
 * HTTP-as-a-function: every HTTP surface in Treesinger — its own handler and
 * the upstream client — is a `Fetch`. That single shape is why the whole system
 * is trivially testable: swap the real `globalThis.fetch` for an in-memory one.
 */
export type Fetch = (request: Request) => Promise<Response>;

/** The slice of `console` we use; a capturing double satisfies it structurally. */
export interface Logger {
    log(...args: unknown[]): void;
    warn(...args: unknown[]): void;
    error(...args: unknown[]): void;
}

/**
 * A non-2xx (or otherwise unusable) upstream response, carrying the HTTP status
 * so callers/handlers can map it back out — `401` → re-auth needed, `403` →
 * entitlement/session-limit. `code` is the upstream `error` field when present.
 */
export class UpstreamError extends Error {
    constructor(
        readonly status: number,
        readonly code: string,
        message: string,
    ) {
        super(message);
        this.name = "UpstreamError";
    }
}

/**
 * Retry transient upstream failures — thrown network errors, `429`, and `5xx` —
 * with exponential backoff. The request is cloned per attempt so a retry can
 * resend a body-bearing POST (a Request body is single-use).
 *
 * Note on the rotating refresh token: a refresh that the server processed but
 * whose response was lost would, on retry, fail with `401` and surface as
 * re-auth-needed — the documented recovery — never a silent double-spend.
 */
export function retryingFetch(
    inner: Fetch,
    opts: { retries?: number; baseDelayMs?: number } = {},
): Fetch {
    const retries = opts.retries ?? 2;
    const baseDelayMs = opts.baseDelayMs ?? 200;
    return async (request) => {
        // Buffer the body once so each attempt sends a fresh, independent Request:
        // a Request body is a single-use stream, so a retry cannot reuse the original.
        const hasBody = request.method !== "GET" && request.method !== "HEAD";
        const body = hasBody ? await request.arrayBuffer() : undefined;
        const build = () => new Request(request.url, { method: request.method, headers: request.headers, body });
        for (let attempt = 0; ; attempt++) {
            try {
                const response = await inner(build());
                if ((response.status === 429 || response.status >= 500) && attempt < retries) {
                    await delay(baseDelayMs * 2 ** attempt);
                    continue;
                }
                return response;
            } catch (error) {
                if (attempt < retries) {
                    await delay(baseDelayMs * 2 ** attempt);
                    continue;
                }
                throw error;
            }
        }
    };
}

/** Log method + URL + status + duration around each call. */
export function loggingFetch(logger: Logger, inner: Fetch): Fetch {
    return async (request) => {
        const started = performance.now();
        try {
            const response = await inner(request);
            logger.log(`${request.method} ${request.url} → ${response.status} (${Math.round(performance.now() - started)}ms)`);
            return response;
        } catch (error) {
            logger.error(`${request.method} ${request.url} → threw`, error);
            throw error;
        }
    };
}

/** The one real timer in the codebase; only ever reached on a retry backoff. */
function delay(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
}
