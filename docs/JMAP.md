# JMAP configuration

> **Status: phase 1 — session resource only**

`yarilo-jmap` is the JMAP (RFC 8620) service binary. Phase 1 ships the HTTPS
listener and the session resource; no data methods are served yet. See
["Not yet implemented"](#not-yet-implemented) for the full boundary.

---

## Listener

| Service key | Default port | Protocol |
|:---|:---|:---|
| `jmap` | `8443` | HTTPS (TLS terminated by the pod) |

`services.jmap` is a standard service block:

| Key | Default | Description |
|:---|:---|:---|
| `enabled` | `false` | Start the JMAP listener. |
| `port` | `8443` | Listener port. |
| `ssl_mode` | `ssl` | JMAP is HTTPS-only; the listener terminates TLS using `general.ssl`. |
| `connection_limit` | `1000` | Max concurrent connections. Enforced by the listener: a connection over the limit is refused. |
| `haproxy_protocol` | `false` | Expect a HAProxy PROXY header ahead of the TLS handshake, so the client address behind a proxy is the real one. |
| `disable_plaintext_auth` | `true` | Reject Basic auth when the connection is not TLS-protected. |

The binary reads its configuration from `$CONFIG` (default
`/etc/yarilo/yarilo.yaml`), honours `LOG_LEVEL`, and exposes health and metrics
on `$TELEMETRY_LISTEN` (`/healthz`, `/readyz`).

---

## `protocol.jmap`

Every key carries the `jmap_` prefix, matching the config keys verbatim.

| Key | Default | Description |
|:---|:---|:---|
| `jmap_base_url` | `""` | Public HTTPS origin clients reach this server on. It prefixes every URL published in the session resource, so behind a proxy or load balancer set the externally visible name, not the pod address. Empty derives the origin from the request `Host` header. |
| `jmap_max_concurrent_requests` | `10` | Max simultaneous API requests per session. |
| `jmap_max_objects_in_get` | `500` | Max objects a single `Foo/get` may return. |
| `jmap_max_objects_in_set` | `500` | Max objects a single `Foo/set` may change. |
| `jmap_max_size_upload` | `"40M"` | Max size of one blob upload. Human-readable (bytes or `K`/`M`/`G`/`T`); quote it. |
| `jmap_max_size_request` | `"10M"` | Max size of one API request body. Human-readable; quote it. |
| `jmap_max_calls_in_request` | `16` | Max method calls in one batched request. |
| `jmap_push_timeout` | `90` | Idle timeout for a push connection, in seconds. Advertised only; unused until the push phase. |

These limits are what the protocol requires the server to publish so clients can
size their batches. They are advertised in the session resource regardless of
whether the corresponding method is served yet.

Example:

```yaml
services:
  jmap:
    enabled: true
    port: 8443
    ssl_mode: ssl
    connection_limit: 1000
    haproxy_protocol: false
    disable_plaintext_auth: true

protocol:
  jmap:
    jmap_base_url: "https://mail.example.com"
    jmap_max_concurrent_requests: 10
    jmap_max_objects_in_get: 500
    jmap_max_objects_in_set: 500
    jmap_max_size_upload: "40M"
    jmap_max_size_request: "10M"
    jmap_max_calls_in_request: 16
    jmap_push_timeout: 90
```

---

## Session resource

```
GET /.well-known/jmap
```

Returns `200` with `Content-Type: application/json` and the session object of
RFC 8620 §2: the authenticated account, the server `capabilities` map with its
published limits, and the API / download / upload / event-source URLs derived
from `jmap_base_url`.

Any other method on the endpoint answers `405`. An unauthenticated or
failed-authentication request answers `401` with a `WWW-Authenticate` header.

---

## Authentication

Both mechanisms authenticate against the same passdb stack as the other
protocols (see [docs/AUTH.md](AUTH.md)).

| Method | Header | Notes |
|:---|:---|:---|
| Bearer | `Authorization: Bearer <token>` | For deployments with an OAuth2/OIDC passdb configured. |
| Basic | `Authorization: Basic <base64(user:pass)>` | Username and password, as for IMAP login. |

Bearer is tried first. A rejected token is **not** retried as Basic, so a bad
token is reported as a token failure, never as a password failure — worth
knowing when reading logs, because the two failures point at different
passdbs.

`disable_plaintext_auth` rejects Basic only when the connection is not
TLS-protected; it never affects Bearer.

There is no session cookie and no login form: every request carries its own
credentials.

---

## Helm

Enable the component and its listener:

```yaml
components:
  jmap:
    enabled: true
    replicas: 2
    listeners:
      jmap:
        enabled: true
        servicePort: 8443
        containerPort: 8443
        disablePlainAuth: true
    connection_limit: 1000
    haproxy_protocol: false
    tls:
      secretName: mail-tls
```

`components.jmap.connection_limit`, `.haproxy_protocol` and
`listeners.jmap.disablePlainAuth` render into the `services.jmap` block of the
generated `yarilo.yaml`; the `protocol.jmap` values render verbatim under
`protocol.jmap`.

JMAP runs as its own Deployment rather than a container in the co-located
backend pod: it terminates client TLS itself and is reached directly by
clients, so it scales on request load instead of the per-user mailbox affinity
that binds the protocol containers to a single pod IP. It gets its own
telemetry port (`components.jmap.telemetryPort`, default `8087`) and a
`<release>-jmap-telemetry` Service alongside the `<release>-jmap` API Service.

---

## Not yet implemented

Phase 1 ships nothing beyond the listener and the session resource. The
following are not served:

- **Data methods** — `Mailbox/*`, `Email/*`, `Thread/*`, `Identity/*`,
  `SearchSnippet/*`, and the API request endpoint that batches them.
- **`/changes` and state tracking** — `Foo/changes`, `Foo/queryChanges`, and
  the per-account state strings they depend on.
- **Blob upload / download** — the `uploadUrl` and `downloadUrl` endpoints.
- **Email submission** — `EmailSubmission/*` (RFC 8621); use SMTP submission.
- **Push** — the event-source endpoint, `PushSubscription/*`, and WebSocket
  push (RFC 8887).

Clients that need mail access today should use IMAP
([docs/IMAP.md](IMAP.md)).
