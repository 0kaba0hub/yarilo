# Services configuration

The `services` section controls which listeners are started and how each one behaves at the network level. Each service is a named block under `services:`. A missing key or `enabled: false` means the listener is not started.

---

## Common fields

All services share the following fields:

| Key | Default | Description |
|:---|:---|:---|
| `enabled` | `false` | Start this listener on startup. |
| `port` | see table | TCP port to bind. |
| `ssl_mode` | — | TLS mode: `ssl` = implicit TLS; `starttls` = plain with STARTTLS upgrade; `no` = plain only. |
| `haproxy_protocol` | `false` | Extract real client IP from HAProxy PROXY header. Uses `general.haproxy` for timeout and trusted nets. |
| `xclient_protocol` | `false` | Accept XCLIENT command from trusted relays. Uses `general.xclient` for trusted nets. |
| `disable_plaintext_auth` | `false` | Reject AUTH (IMAP/SMTP) or USER/PASS (POP3) unless the connection is TLS-protected. |
| `ssl` | — | Per-service SSL override. Same fields as `general.ssl`. If set, overrides only the specified fields. |

---

## Listeners

| Key | Port | ssl_mode | Protocol doc |
|:---|:---|:---|:---|
| `imaps` | `993` | `ssl` | [IMAP.md](IMAP.md) |
| `imap` | `143` | `starttls` | [IMAP.md](IMAP.md) |
| `smtp` | `25` | `no` | [SMTP.md](SMTP.md) |
| `submission` | `587` | `starttls` | [SMTP.md](SMTP.md) |
| `submissions` | `465` | `ssl` | [SMTP.md](SMTP.md) |
| `pop3` | `110` | `starttls` | [POP3.md](POP3.md) |
| `pop3s` | `995` | `ssl` | [POP3.md](POP3.md) |

---

## Examples

### Minimal: IMAPS only

```yaml
services:
  imaps:
    enabled: true
    port: 993
    ssl_mode: ssl
```

### IMAP + IMAPS behind HAProxy

```yaml
services:
  imaps:
    enabled: true
    port: 993
    ssl_mode: ssl
    haproxy_protocol: true
  imap:
    enabled: true
    port: 143
    ssl_mode: starttls
    haproxy_protocol: true
    disable_plaintext_auth: true
```

### Full mail server (IMAP + SMTP + POP3)

```yaml
services:
  imaps:
    enabled: true
    port: 993
    ssl_mode: ssl
  imap:
    enabled: true
    port: 143
    ssl_mode: starttls
    disable_plaintext_auth: true
  smtp:
    enabled: true
    port: 25
    ssl_mode: no
  submission:
    enabled: true
    port: 587
    ssl_mode: starttls
    disable_plaintext_auth: true
  submissions:
    enabled: true
    port: 465
    ssl_mode: ssl
    disable_plaintext_auth: true
  pop3s:
    enabled: true
    port: 995
    ssl_mode: ssl
```

### Per-service TLS certificate override

```yaml
services:
  imaps:
    enabled: true
    port: 993
    ssl_mode: ssl
    ssl:
      tls_cert: /etc/ssl/mail.example.com/cert.pem
      tls_key:  /etc/ssl/mail.example.com/key.pem
```
