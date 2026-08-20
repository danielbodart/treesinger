# --- build: compile a standalone binary with Bun ---------------------------
FROM oven/bun:1.3.14 AS build
WORKDIR /app
# Install deps against the lockfile first for layer caching.
COPY package.json bun.lock .npmrc ./
RUN bun install --frozen-lockfile
COPY tsconfig.json ./
COPY src ./src
RUN bun build --compile --target=bun-linux-x64 --outfile treesinger src/main.ts

# --- runtime: ship just the binary on a slim glibc base --------------------
FROM debian:bookworm-slim
# The Bun-compiled binary links glibc + libstdc++; the slim base carries both.
# Runs unprivileged; the token store lives on a mounted volume it owns.
RUN useradd --system --create-home --uid 10001 treesinger
COPY --from=build /app/treesinger /usr/local/bin/treesinger
USER treesinger
ENV TREESINGER_STORE=/data/tokens.json \
    TREESINGER_PORT=8080
VOLUME ["/data"]
EXPOSE 8080
# Self-contained readiness probe (no curl in the image) — backs compose
# `depends_on: { condition: service_healthy }`.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD ["treesinger", "health"]
ENTRYPOINT ["treesinger"]
CMD ["serve"]
