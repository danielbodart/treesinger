# Running Treesinger in Docker

The image is `ghcr.io/danielbodart/treesinger`, for `amd64` and `arm64`, tagged
`:latest` and with each exact version, so a deployment can be pinned and rolled
back. In a container the broker listens on TCP `:8080`, and its boundary is the
compose network: endpoint auth is off unless `TREESINGER_BEARER_TOKEN` is set.
The variables are in the [README](../README.md#configuration). To run it
natively instead, see [Running Treesinger on systemd](systemd.md).

## Docker / compose

```yaml
services:
  treesinger:
    image: ghcr.io/danielbodart/treesinger:latest
    volumes:
      - treesinger-data:/data          # holds the token store
    networks: [fleet]
    # image ships a self-contained HEALTHCHECK (treesinger health)

  hytale-server:
    image: ghcr.io/danielbodart/chandlery-hytale:latest
    depends_on:
      treesinger:
        condition: service_healthy     # cold-start ordering; broker covers restarts
    networks: [fleet]

volumes:
  treesinger-data:
networks:
  fleet:
```

The consumer image fetches a pair from `POST /v1/session` at boot and passes them
as the game's own `--session-token` / `--identity-token`. No shared secret;
network isolation is the boundary.

## Login

Run `login` once against the same volume before the first `serve`:

```sh
docker run --rm -it -v treesinger-data:/data ghcr.io/danielbodart/treesinger login
```
