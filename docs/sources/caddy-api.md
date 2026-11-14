# Caddy Admin API

dnsweaver can read hostnames from a running Caddy server through its
[admin API](https://caddyserver.com/docs/api). This works with both
`Caddyfile` and JSON configurations. For
[caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy)
labels, use the [Caddy Labels](caddy.md) source instead.

## Enabling Caddy Admin API

Add `caddy-api` to `DNSWEAVER_SOURCES` and point dnsweaver at the admin API:

```yaml
environment:
  - DNSWEAVER_SOURCES=caddy-api
  - DNSWEAVER_CADDY_API_URL=http://caddy:2019
```

`DNSWEAVER_CADDY_API_URL` is required. This source doesn't need the Docker
socket, so `DNSWEAVER_PLATFORM=none` is fine if it's your only source.

## What Gets Discovered

On every reconcile, dnsweaver reads `GET /config/` and collects every `host`
matcher, including those nested in `handle` blocks:

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

This yields `app.example.com`, `www.example.com`, and `git.example.com`.
Wildcards, placeholders (`{env.SITE}`), IP addresses, and address-only sites
(`:8080`) are skipped.

## Exposing the Admin API

Caddy's admin API listens on `localhost:2019` by default. To reach it from
the dnsweaver container, put both on a shared Docker network and bind the
admin API to all interfaces:

```caddyfile
{
    admin :2019 {
        origins caddy:2019 localhost:2019 :2019
    }
}
```

The `:2019` origin keeps `caddy reload` working inside the Caddy container.

!!! warning "Keep the admin API private"
    The admin API is unauthenticated and can rewrite Caddy's config.
    Don't publish port 2019, and attach only dnsweaver to the shared network.

## Caveats

- **Unreachable API is not an empty config.** If the admin API errors, the
  reconcile is marked incomplete and no records are deleted.
- **Changing `admin` needs a restart.** `caddy reload` checks the *running*
  config's `origins`, so recreate the container the first time.
- **Hostnames only.** Each hostname gets the matching provider's default
  record type (typically `A`).
