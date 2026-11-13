# Caddy Admin API

The `caddy-api` source reads hostnames from a running Caddy server through its
built-in [admin API](https://caddyserver.com/docs/api). It works with any
Caddy configuration (`Caddyfile` or JSON), because Caddy adapts both to the
same JSON model.

If you configure Caddy with
[caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy)
labels instead, use the [Caddy Labels](caddy.md) source.

## Configuration

```yaml
environment:
  - DNSWEAVER_SOURCES=caddy-api
  - DNSWEAVER_CADDY_API_URL=http://caddy:2019
  - DNSWEAVER_PLATFORM=none   # no Docker socket needed for this source
```

When using a config file, list `caddy-api` under `sources:` and set
`DNSWEAVER_CADDY_API_URL` in the environment.

| Variable | Required | Description |
|----------|----------|-------------|
| `DNSWEAVER_CADDY_API_URL` | Yes | Base URL of the Caddy admin API (`http://` or `https://`, no path) |

## What Gets Discovered

dnsweaver sends a read-only `GET /config/` on every reconcile, so Caddy
changes reach DNS within `DNSWEAVER_RECONCILE_INTERVAL` (default `60s`). It
collects the values of every `host` matcher, including matchers nested inside
`handle` blocks. Given:

```caddyfile
app.example.com, www.example.com {
    reverse_proxy app:80
}

*.example.com {
    @git host git.example.com
    handle @git {
        reverse_proxy gitea:3000
    }
}
```

dnsweaver discovers `app.example.com`, `www.example.com`, and
`git.example.com`. It skips anything that can't become a host record:
wildcards (`*.example.com`), placeholders (`{env.SITE}`), IP addresses, and
sites without a hostname (`:8080`).

Discovered hostnames still go through each provider's `DOMAINS` /
`EXCLUDE_DOMAINS` filters.

## Reaching the Admin API from Another Container

Caddy's admin API listens on `localhost:2019` by default, which other
containers can't reach. Put Caddy and dnsweaver on a shared Docker network
and have Caddy listen on all interfaces:

```caddyfile
{
    admin :2019 {
        origins caddy:2019 localhost:2019 :2019
    }
}
```

The `:2019` entry in `origins` keeps `caddy reload` working inside the Caddy
container. Without it, reloads fail with `HTTP 403 ... not allowed to access
from origin`.

```yaml
# Caddy compose
services:
  caddy:
    image: caddy:2
    container_name: caddy
    networks: [caddy-admin]
    volumes:
      - ./caddy:/etc/caddy

networks:
  caddy-admin:
    name: caddy-admin
```

```yaml
# dnsweaver compose
services:
  dnsweaver:
    networks: [caddy-admin]
    environment:
      - DNSWEAVER_PLATFORM=none
      - DNSWEAVER_SOURCES=caddy-api
      - DNSWEAVER_CADDY_API_URL=http://caddy:2019

networks:
  caddy-admin:
    external: true
```

Check that dnsweaver will be able to reach it:

```bash
docker run --rm --network caddy-admin curlimages/curl -s http://caddy:2019/config/
```

!!! warning "Keep the admin API private"
    The admin API has no authentication and can rewrite Caddy's entire
    config. dnsweaver only reads from it, but anything else that can reach
    it can write. Don't publish port 2019, and attach only dnsweaver to the
    shared network.

!!! tip "Changing the admin address"
    `caddy reload` is checked against the *running* config's `origins`.
    When you first change the `admin` block, restart the container
    (`docker compose up -d --force-recreate caddy`) instead of reloading.
    If you bind-mount a single `Caddyfile`, mount its directory instead so
    edits are visible inside the container.

## Failure Behavior

If the admin API is unreachable or returns an error, the reconcile cycle is
marked incomplete and **no records are deleted**, so restarting Caddy won't
wipe your DNS. Removing a site from Caddy and reloading deletes its record on
the next cycle.
