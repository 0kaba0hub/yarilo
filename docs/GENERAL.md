# General configuration

The `general` section defines infrastructure settings shared across all listeners. Individual services can override `ssl` per-listener with their own `ssl:` block.

---

## `general.ssl`

Shared TLS certificate. Used by every TLS-enabled listener that does not define its own `ssl:` override.

| Key | Default | Description |
|:---|:---|:---|
| `tls_cert` | — | Path to PEM certificate (or full chain). `${ENV_VAR}` expanded at startup. |
| `tls_key` | — | Path to PEM private key matching `tls_cert`. |
| `tls_alt_cert` | — | Optional second certificate (e.g. ECDSA) for dual-cert SNI. |
| `tls_alt_key` | — | Private key for `tls_alt_cert`. |
| `tls_min_version` | `TLS1.2` | Minimum TLS version: `TLS1.2` \| `TLS1.3`. |
| `prefer_server_ciphers` | `false` | Use server cipher-suite preference order. |

**TLS ALPN matching (the reference parity).** TLS listeners advertise ALPN protocol identifiers per IANA RFC 7301: `imap` for IMAP/IMAPS, `pop3` for POP3/POP3S, `smtp` for Submission/Submissions. Clients that send ALPN must match — mismatching connections are refused. Clients without ALPN are accepted (backward-compatibility). LMTP is internal-only and does not enforce ALPN.

```yaml
general:
  ssl:
    tls_cert: /etc/ssl/yarilo/cert.pem
    tls_key:  /etc/ssl/yarilo/key.pem
    tls_alt_cert: /etc/ssl/yarilo/ecdsa.pem   # optional
    tls_alt_key:  /etc/ssl/yarilo/ecdsa.key
    tls_min_version: TLS1.2
    prefer_server_ciphers: false
```

`${ENV_VAR}` substitution is supported in all path values — no secrets in config files.

---

## `general.haproxy`

HAProxy PROXY protocol v1/v2. When `haproxy_protocol: true` is set on a service, the real client IP is extracted from the `PROXY` header. Connections from addresses outside `trusted_nets` ignore the header (the TCP source IP is used instead).

| Key | Default | Description |
|:---|:---|:---|
| `timeout` | `3` | Seconds to wait for the PROXY header. Connection closed if header not received in time. |
| `trusted_nets` | `["127.0.0.1/32", "10.0.0.0/8"]` | CIDRs whose PROXY headers are accepted. |

```yaml
general:
  haproxy:
    timeout: 3
    trusted_nets:
      - 127.0.0.1/32
      - 10.0.0.0/8
      - 172.16.0.0/12
```

Enable per-listener with `haproxy_protocol: true` in the service config. See [SERVICES.md](SERVICES.md).

---

## `general.xclient`

SMTP XCLIENT command for trusted relay infrastructure. A relay behind which yarilo sits can pass the real client IP, hostname, and helo string via XCLIENT before the mail transaction begins. Only connections from `trusted_nets` are allowed to send XCLIENT.

**Supported XCLIENT attributes:** `ADDR`, `PORT`, `HELO`, `LOGIN`, `PROTO`, `SESSION`, `TTL`, `FORWARD`, plus the reference extensions `DESTADDR` (alias: `DESTIP`) and `DESTPORT` for the destination the client originally connected to (load-balancer awareness).

| Key | Default | Description |
|:---|:---|:---|
| `trusted_nets` | `["127.0.0.1/32", "10.0.0.0/8"]` | CIDRs allowed to issue XCLIENT. |

```yaml
general:
  xclient:
    trusted_nets:
      - 127.0.0.1/32
      - 10.0.0.0/8
```

Enable per-listener with `xclient_protocol: true` in the service config. See [SERVICES.md](SERVICES.md).

---

## `general.limits`

| Key | Default | Description |
|:---|:---|:---|
| `mail_max_userip_connections` | `10` | Max simultaneous connections per user+IP pair, counted across all IMAP and POP3 listeners. `0` = unlimited. |

```yaml
general:
  limits:
    mail_max_userip_connections: 10
```
