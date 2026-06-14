# LMTP configuration

LMTP (RFC 2033) is the Local Mail Transfer Protocol used for final delivery from an external MTA (Postfix, Exim, etc.) into the yarilo mailbox. It operates on port 24 and returns per-recipient status codes, making it a drop-in replacement for SMTP-based local delivery.

## Architecture

yarilo's LMTP stack is split into two components:

| Binary | Role |
|:---|:---|
| `yarilo-lmtp-login` | MTA-facing proxy. Accepts one LMTP session from the MTA, tracks each recipient in anvil (`CONNECT`), issues a service-scoped SESSION token per recipient via yarilo-auth master protocol, then at DATA time fans out one preamble TCP connection to `yarilo-lmtp` per recipient. |
| `yarilo-lmtp` | Backend delivery. Accepts preamble connections (`YARILO\t...TOKEN=...\n`), verifies the token with yarilo-auth (`VERIFY`, `service=lmtp` enforced), and delivers to the mailbox. No XCLIENT, no HAProxy, no direct anvil access. |

**Why fan-out?** Each recipient may hash to a different backend pod (in director mode). A single multi-recipient DATA payload is split per recipient so each backend receives exactly the messages it is responsible for. Per-recipient status codes are merged and returned to the MTA.

**Token scoping.** SESSION tokens are issued with `service=lmtp`. The `yarilo-lmtp` `PreambleListener` rejects tokens issued for any other service (imap, pop3, smtp), preventing cross-service replay.

See [SERVICES.md](SERVICES.md) for listener-level settings (`port`, `ssl_mode`).

---

## `protocol.lmtp`

| Key | Default | Description |
|:---|:---|:---|
| `login_greeting` | `Yarilo ready.` | Text appended to the `220` banner. |
| `add_received_header` | `true` | Prepend a `Received:` header to every delivered message. |
| `save_to_detail_mailbox` | `false` | When `true`, `user+folder@domain` delivers to the `folder` mailbox instead of `INBOX`. |
| `hdr_delivery_address` | `final` | Controls the `Delivered-To:` header: `none` — omit; `final` — address after detail stripping; `original` — RCPT TO address as received. |
| `verbose_replies` | `false` | Include diagnostic details in 4xx/5xx error responses (useful for debugging; disable in production). |
| `user_concurrency_limit` | `0` | Maximum concurrent deliveries per user. `0` = unlimited. |
| `read_timeout` | `300` | Per-command read timeout in seconds. |
| `write_timeout` | `300` | Per-command write timeout in seconds. |
| `client_workarounds` | — | List of client compatibility workarounds (see below). |

```yaml
protocol:
  lmtp:
    login_greeting: "Yarilo ready."
    add_received_header: true
    save_to_detail_mailbox: false
    hdr_delivery_address: final
    verbose_replies: false
    user_concurrency_limit: 5
    read_timeout: 300
    write_timeout: 300
```

---

## `hdr_delivery_address`

Controls the `Delivered-To:` header prepended before storing the message.

| Value | Behaviour |
|:---|:---|
| `none` | No `Delivered-To:` header is added. |
| `final` | `Delivered-To:` shows the address after subaddress stripping (`alice@example.com`). Default. |
| `original` | `Delivered-To:` shows the RCPT TO address as received (`alice+tag@example.com`). |

---

## `client_workarounds`

A list of compatibility shims for non-conformant MTA clients. Unknown entries are silently ignored (Dovecot behaviour).

| Name | Effect |
|:---|:---|
| `whitespace-before-path` | Allows whitespace between the command verb and `<path>`: `MAIL FROM: <user@example.com>`. |
| `mailbox-for-path` | Allows a bare mailbox name without a domain in RCPT TO: `RCPT TO:<alice>`. |

```yaml
protocol:
  lmtp:
    client_workarounds:
      - whitespace-before-path
      - mailbox-for-path
```

---

## `protocol.lmtp.proxy`

Proxy mode is active only on **director** nodes. The director's consistent-hashing ring (built from `general` backend settings) routes each recipient to the correct backend. Backend nodes always deliver locally — `protocol.lmtp.proxy` has no effect on them.

When multiple recipients hash to different backends, deliveries run in parallel and per-recipient status codes are merged before replying to the MTA.

| Key | Default | Description |
|:---|:---|:---|
| `proxy.timeout` | `125` | Per-backend connect + transaction timeout in seconds. |

```yaml
protocol:
  lmtp:
    proxy:
      timeout: 60
```

---

## Listener (service-level settings)

```yaml
services:
  lmtp:
    enabled: true
    port: 24
    ssl_mode: no    # no | starttls | ssl
```

The real client IP and recipient identity are carried in the YARILO preamble from `yarilo-lmtp-login`; no HAProxy or XCLIENT handling on the backend.

---

## Example: backend node (yarilo-lmtp)

```yaml
services:
  lmtp:
    enabled: true
    port: 24
    ssl_mode: no

protocol:
  lmtp:
    add_received_header: true
    hdr_delivery_address: final
    user_concurrency_limit: 5
    read_timeout: 300
    write_timeout: 300
```

The backend listens only for preamble connections from `yarilo-lmtp-login`. MTAs connect to `yarilo-lmtp-login`, not directly to this port.

## `lmtp_login_service`

Configuration for `yarilo-lmtp-login`:

| Key | Default | Description |
|:---|:---|:---|
| `lmtp_login_service.backend_addr` | — | Address of the `yarilo-lmtp` backend (e.g. `yarilo-lmtp.yarilo.svc.cluster.local:24`). Required. |

```yaml
lmtp_login_service:
  backend_addr: "yarilo-lmtp.yarilo.svc.cluster.local:24"
```

Enable the component in Helm values:

```yaml
components:
  lmtpLogin:
    enabled: true
    backendAddr: "yarilo-lmtp.yarilo.svc.cluster.local:24"
```

Postfix `main.cf`:

```
mailbox_transport = lmtp:inet:[yarilo-lmtp-login.yarilo.svc.cluster.local]:24
```

---

## Example: director proxy mode (yarilo-lmtp inside director)

```yaml
services:
  lmtp:
    enabled: true
    port: 24
    ssl_mode: no

protocol:
  lmtp:
    proxy:
      timeout: 60
```

In director mode, `yarilo-lmtp-login` points its `backend_addr` at the director's LMTP proxy
service. The director routes each recipient via consistent hashing; failed backends return `451`
and the MTA retries later.

