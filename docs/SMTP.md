# SMTP configuration

Yarilo runs three SMTP listeners from a single server instance:

| Service key | Port | Role |
|:---|:---|:---|
| `smtp` | `25` | MX inbound — receives mail from the internet. No AUTH. |
| `submission` | `587` | Outbound submission — requires AUTH PLAIN, STARTTLS. |
| `submissions` | `465` | Outbound submission — requires AUTH PLAIN, implicit TLS. |

See [SERVICES.md](SERVICES.md) for listener-level settings (port, ssl_mode, haproxy_protocol, disable_plaintext_auth, etc.).

---

## `protocol.smtp`

Protocol-level behaviour shared across all three SMTP listeners.

| Key | Default | Description |
|:---|:---|:---|
| `hostname` | system hostname | EHLO/HELO banner and `Message-ID` domain. |
| `max_message_size` | `41943040` | Maximum accepted message size in bytes (default 40 MiB). |
| `max_line_length` | `4096` | Maximum SMTP command or DATA line length in bytes. |
| `recipient_delimiter` | `+` | Subaddress separator: `user+tag@domain` → `user@domain`. Empty = disabled. |

### `milters`

External milter filters (e.g. rspamd, OpenDKIM). Checked before internal SPF / DKIM / DMARC processing. A milter `5xx` rejection returns `550 5.7.1` to the sender. Milter unavailability is fail-open (mail continues).

| Key | Default | Description |
|:---|:---|:---|
| `milters[].socket` | — | Milter socket address: `unix:/path/to/sock` or `tcp:host:port`. |
| `milters[].timeout` | `30` | Milter response timeout in seconds. |

```yaml
protocol:
  smtp:
    hostname: mail.example.com
    max_message_size: 41943040
    max_line_length: 4096
    recipient_delimiter: "+"
    milters:
      - socket: unix:/run/rspamd/milter.sock
        timeout: 30
      - socket: tcp:127.0.0.1:11332
        timeout: 10
```

---

## Inbound pipeline (port 25)

```
connect
  → external milters
  → SPF check        (if spf.enabled)
  → DKIM verify      (if dkim.verify)
  → DMARC evaluate   (if dmarc.enabled)
  → LMTP local delivery
```

A DMARC `reject` disposition returns `550 5.7.1` and the message is dropped.

---

## Submission pipeline (port 587 / 465)

```
connect
  → STARTTLS / TLS
  → AUTH PLAIN
  → external milters
  → DKIM sign        (if dkim.sign)
  → relay queue      (phase 4)
```

Submission requires AUTH PLAIN. DKIM signing uses the private key for the sender domain — see [DKIM.md](DKIM.md).

---

## SPF

```yaml
spf:
  enabled: true
```

SPF result is passed to the DMARC evaluator. No standalone rejection on SPF failure — DMARC policy governs disposition.

---

## DMARC

```yaml
dmarc:
  enabled: true
```

Evaluates SPF alignment and DKIM alignment against the RFC 5322 `From` domain. Enforces `reject` and `quarantine` policies (quarantine is treated as reject in this implementation).
