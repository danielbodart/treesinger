# Built on the *build* platform whatever the target, and cross-compiled to the
# target architecture by Bun itself — no QEMU, so it costs seconds not minutes.
FROM --platform=$BUILDPLATFORM oven/bun:1-alpine AS build

WORKDIR /app

COPY package.json bun.lock .npmrc ./
RUN bun install --frozen-lockfile

COPY tsconfig.json ./
COPY src ./src

# Computed by run.ts from the git history, which is not in the build context —
# so it is passed in rather than worked out here. A build with no --build-arg
# still works and says "development", which is the truth about that build.
ARG VERSION=development

ARG TARGETARCH
RUN target="bun-linux-$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo x64)-musl" && \
    bun build --compile --minify --define "TREESINGER_VERSION=\"$VERSION\"" --target="$target" src/main.ts --outfile treesinger

FROM alpine:3

# An ARG does not survive into the next stage, so it is declared again.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="treesinger" \
      org.opencontainers.image.description="Central authentication broker for Hytale dedicated servers" \
      org.opencontainers.image.source="https://github.com/danielbodart/treesinger" \
      org.opencontainers.image.licenses="Apache-2.0"

# libstdc++   - Bun's compiled binary links the C++ runtime.
# ca-certificates - for the outbound HTTPS to Hypixel's OAuth/session services.
# tini        - a real PID 1 that forwards signals for a clean `docker stop`.
RUN apk add --no-cache libstdc++ ca-certificates tini

# Runs unprivileged; the token store lives on a mounted volume it owns.
RUN adduser -S -u 10001 treesinger
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

# tini as PID 1, then treesinger. `--` stops tini reading treesinger's flags.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/treesinger"]
CMD ["serve"]
