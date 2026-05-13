# LMTP configuration

LMTP (RFC 2033) is the Local Mail Transfer Protocol used for final delivery from an external MTA (Postfix, Exim, etc.) into the yarilo mailbox. It operates on port 24 or a Unix socket and returns per-recipient status codes, making it a drop-in replacement for SMTP-based local delivery.

| Service key | Port | Role |
|:---|:---|:---|
| `lmtp` | `24` | Local delivery — receives from MTA, delivers to mailboxes. No AUTH. Loopback only. |

See [SERVICES.md](SERVICES.md) for listener-level settings (`port`, `ssl_mode`, `haproxy_protocol`, `xclient_protocol`).

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

Proxy mode turns the LMTP listener into a **director**: instead of delivering locally, it routes each recipient to the correct backend node via consistent hashing. This is used on director-tier nodes; backend nodes run LMTP in local delivery mode.

| Key | Default | Description |
|:---|:---|:---|
| `proxy.enabled` | `false` | Enable director mode. When `true`, all RCPT TO addresses are routed to backends. |
| `proxy.timeout` | `125` | Per-backend connect + transaction timeout in seconds. |
| `proxy.backends[].host` | — | Backend hostname or IP address. |
| `proxy.backends[].port` | `24` | Backend LMTP port. |

When multiple recipients hash to different backends, deliveries run in parallel and per-recipient status codes are merged before replying to the MTA.

```yaml
protocol:
  lmtp:
    proxy:
      enabled: true
      timeout: 60
      backends:
        - host: backend1.internal
          port: 24
        - host: backend2.internal
          port: 24
        - host: backend3.internal
          port: 24
```

---

## Listener (service-level settings)

```yaml
services:
  lmtp:
    enabled: true
    port: 24
    ssl_mode: no             # no | starttls | ssl
    haproxy_protocol: true   # extract real IP from HAProxy PROXY header
    xclient_protocol: true   # accept XCLIENT from Postfix (real client IP + hostname)
```

HAProxy and XCLIENT trusted nets come from `general.haproxy` and `general.xclient` respectively (see [GENERAL.md](GENERAL.md)).

---

## Example: local delivery (backend node)

```yaml
services:
  lmtp:
    enabled: true
    port: 24
    ssl_mode: no
    haproxy_protocol: false
    xclient_protocol: true

protocol:
  lmtp:
    add_received_header: true
    hdr_delivery_address: final
    user_concurrency_limit: 5
    read_timeout: 300
    write_timeout: 300
```

Postfix `main.cf`:

```
mailbox_transport = lmtp:inet:localhost:24
```

---

## Example: director proxy node

```yaml
services:
  lmtp:
    enabled: true
    port: 24
    ssl_mode: no
    haproxy_protocol: false
    xclient_protocol: false

protocol:
  lmtp:
    proxy:
      enabled: true
      timeout: 60
      backends:
        - host: 10.0.1.10
        - host: 10.0.1.11
        - host: 10.0.1.12
```

The proxy node forwards mail to the backend determined by consistent hashing on the recipient username. Failed backends return a `451` temporary error; the MTA retries later.

---

## Example: LMTP over STARTTLS with HAProxy

```yaml
services:
  lmtp:
    enabled: true
    port: 24
    ssl_mode: starttls
    haproxy_protocol: true

general:
  ssl:
    tls_cert: /etc/ssl/yarilo/cert.pem
    tls_key:  /etc/ssl/yarilo/key.pem
  haproxy:
    trusted_nets: ["127.0.0.1/32", "10.0.0.0/8"]
    timeout: 3
```
