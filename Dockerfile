# Built on the *build* platform whatever the target, and cross-compiled to the
# target architecture by Go itself — no QEMU, so it costs seconds not minutes.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build

WORKDIR /src

# No requirements in go.mod, so there is nothing to download first.
COPY go.mod ./
COPY main.go ./
COPY internal ./internal

# Computed by ./run from the git history, which is not in the build context —
# so it is passed in rather than worked out here. A build with no --build-arg
# still works and says "development", which is the truth about that build.
ARG VERSION=development

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/danielbodart/treesinger/internal/broker.Version=$VERSION" \
    -o /out/treesinger .

# The token store's directory, owned by the user the broker runs as. A named
# volume mounted here starts as a copy of it, so the broker can write there.
RUN mkdir -p /out/data && chown 10001:10001 /out/data

# Nothing but the binary, the CA bundle and the store's directory. A static Go
# binary needs no libc, and no tini either: it handles SIGTERM itself (serve
# stops on it and exits), and as PID 1 it starts no children to reap - sd_notify
# is a datagram, not a systemd-notify process. The healthcheck is the binary
# too, so there is no shell or curl to want.
FROM scratch

# An ARG does not survive into the next stage, so it is declared again.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="treesinger" \
      org.opencontainers.image.description="Central authentication broker for Hytale dedicated servers" \
      org.opencontainers.image.source="https://github.com/danielbodart/treesinger" \
      org.opencontainers.image.licenses="Apache-2.0"

# For the outbound HTTPS to Hypixel's OAuth/session services.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/treesinger /usr/local/bin/treesinger
COPY --from=build /out/data /data

# Runs unprivileged, by number: an empty image has no /etc/passwd to name it in.
USER 10001:10001
ENV TREESINGER_STORE=/data/tokens.json \
    TREESINGER_PORT=8080
VOLUME ["/data"]
EXPOSE 8080

# Self-contained readiness probe (no curl in the image) — backs compose
# `depends_on: { condition: service_healthy }`.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/treesinger", "health"]

ENTRYPOINT ["/usr/local/bin/treesinger"]
CMD ["serve"]
