# SMTP configuration

Yarilo runs three SMTP listeners from a single server instance:

| Service key | Port | Role |
|:---|:---|:---|
| `smtp` | `25` | MX inbound — receives mail from the internet. No AUTH. |
| `submission` | `587` | Outbound submission — AUTH PLAIN required, STARTTLS. |
| `submissions` | `465` | Outbound submission — AUTH PLAIN required, implicit TLS. |

See [SERVICES.md](SERVICES.md) for listener-level settings (`port`, `ssl_mode`, `haproxy_protocol`, `xclient_protocol`, `disable_plaintext_auth`).

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

```yaml
protocol:
  smtp:
    hostname: mail.example.com
    max_message_size: 41943040
    max_line_length: 4096
    max_recipients: 100
    recipient_delimiter: "+"
```

---

## Inbound (port 25)

Accepts mail from external MTAs. No AUTH. Subaddress extension is stripped using `recipient_delimiter` before delivery.

After DATA the message is passed to the internal delivery engine (`lmtp.Deliverer`) which writes directly to the mailbox — no outbound network connection is made.

---

## Submission (port 587 / 465)

Accepts mail from MUAs. AUTH PLAIN is required (the only advertised mechanism). After successful authentication and DATA, the message is forwarded to the configured upstream MTA via `protocol.smtp.relay`. If `relay.host` is empty, submission returns `451`.

`disable_plaintext_auth: true` in the service config blocks AUTH on unencrypted connections; pair it with `ssl_mode: starttls` (port 587) or `ssl_mode: ssl` (port 465).

---

## Relay (`protocol.smtp.relay`)

Configures the upstream MTA for submission. One TCP connection per message; any transport error returns `451 4.4.0` to the MUA.

| Key | Default | Description |
|:---|:---|:---|
| `relay.host` | — | Upstream MTA hostname or IP. Empty = relay disabled, submission returns 451. |
| `relay.port` | `25` | Upstream MTA port. |
| `relay.user` | — | SASL PLAIN username sent to upstream. Empty = no AUTH. |
| `relay.password` | — | SASL PLAIN password. Supports `${ENV_VAR}`. |
| `relay.ssl` | `no` | Transport security to upstream: `no` \| `starttls` \| `smtps`. |
| `relay.ssl_verify` | `true` | Verify upstream TLS certificate. |
| `relay.trusted` | `false` | Send XCLIENT to upstream with the MUA's real IP (requires upstream to advertise XCLIENT). |
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
