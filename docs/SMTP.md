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
| `max_recipients` | `0` | Maximum recipients per message. `0` = unlimited. |
| `recipient_delimiter` | `+` | Subaddress separator: `user+tag@domain` → `user@domain`. Empty = disabled. |

### `milters`

External milter filters (e.g. rspamd, OpenDKIM). A milter `5xx` rejection returns `550 5.7.1` to the sender. Milter unavailability is fail-open (mail continues).

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
    max_recipients: 100
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
  → LMTP local delivery
```

---

## Submission pipeline (port 587 / 465)

```
connect
  → STARTTLS / TLS
  → AUTH PLAIN
  → external milters
  → relay (submission_relay_*)
```

Submission requires AUTH PLAIN. The message is forwarded to the configured relay server unchanged — signing and filtering are handled by external milters (e.g. rspamd, OpenDKIM).

---

## Relay (`protocol.smtp.relay`)

Mirrors Dovecot's `submission_relay_*` settings. One connection per message; fail-closed (451) on any transport error.

| Key | Default | Description |
|:---|:---|:---|
| `relay.host` | — | Relay hostname. Empty = relay disabled (451 returned). |
| `relay.port` | `25` | Relay TCP port. |
| `relay.user` | — | SASL PLAIN username. Empty = no AUTH. |
| `relay.password` | — | SASL PLAIN password. Supports `${ENV_VAR}`. |
| `relay.ssl` | `no` | Transport security: `no` \| `smtps` \| `starttls`. |
| `relay.ssl_verify` | `true` | Verify relay TLS certificate. |
| `relay.trusted` | `false` | Send XCLIENT to relay (Postfix) with real client IP. |
| `relay.connect_timeout` | `30` | TCP connect timeout in seconds. |
| `relay.command_timeout` | `300` | Per-command timeout in seconds. |

```yaml
protocol:
  smtp:
    relay:
      host: smtp.example.com
      port: 587
      user: relay-user
      password: "${RELAY_PASSWORD}"
      ssl: starttls
      ssl_verify: true
      trusted: false
      connect_timeout: 30
      command_timeout: 300
```
