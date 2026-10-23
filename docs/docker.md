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

Log in once. In a container there is no admin socket, so `treesinger login`
runs the device flow itself, writing the store at `TREESINGER_STORE`
(`/data/tokens.json`, on the volume) - it says it is logging in locally.

The first login can be made in the running container, which needs no restart
after: a broker that has never been logged in holds nothing that could go
stale, and it reads the store on every request. (`docker exec` runs as the
image's user, so the store keeps its owner.)

```sh
docker exec -it <container> treesinger login
```

Any later login (to another account, or replacing one the broker is using)
is made with the broker stopped, against the same volume, and the broker
started again after:

```sh
docker stop <container>
docker run --rm -it -v treesinger-data:/data ghcr.io/danielbodart/treesinger login
docker start <container>
```

A login run beside a broker that is using the store is a second process
writing it: a refresh the broker has in flight can land after the login and
put the old tokens back, and the broker keeps the old account's profiles (so
mints for the wrong one) for up to an hour. Under systemd, `sudo treesinger
login` goes through the broker itself and has neither problem.
