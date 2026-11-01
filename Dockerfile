# The binary `mise run dist` built for the target architecture, and nothing
# compiled here: the release binaries and every image come from the one compile,
# by the Go mise.toml pins, so they cannot disagree.
#
# Only what an empty image cannot make for itself comes from Alpine, pinned by
# digest so a rebuild cannot change it with no commit to say so; the weekly
# update moves the digest. Its stage runs on the build platform whatever the
# target, since nothing taken from it depends on the architecture.
FROM --platform=$BUILDPLATFORM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS base

# The token store's directory, owned by the user the broker runs as. A named
# volume mounted here starts as a copy of it, so the broker can write there.
RUN mkdir -p /out/data && chown 10001:10001 /out/data

# Nothing but the binary, the CA bundle and the store's directory. A static Go
# binary needs no libc, and no tini either: it handles SIGTERM itself (serve
# stops on it and exits), and as PID 1 it starts no children to reap - sd_notify
# is a datagram, not a systemd-notify process. The healthcheck is the binary
# too, so there is no shell or curl to want.
FROM scratch

# Computed by ./run from the git history and passed in, as dist/ was built
# with it. These labels are how `docker inspect` answers what a pulled image
# actually is, which matters when every published tag also exists as :latest.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="treesinger" \
      org.opencontainers.image.description="Central authentication broker for Hytale dedicated servers" \
      org.opencontainers.image.source="https://github.com/danielbodart/treesinger" \
      org.opencontainers.image.licenses="Apache-2.0"

# For the outbound HTTPS to Hypixel's OAuth/session services.
COPY --from=base /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=base /out/data /data

# TARGETVARIANT is empty for both architectures released, so this is
# dist/linux-amd64 or dist/linux-arm64.
ARG TARGETARCH
ARG TARGETVARIANT
COPY dist/linux-${TARGETARCH}${TARGETVARIANT}/treesinger /usr/local/bin/treesinger

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
