# Sieve Mail Filtering

Yarilo implements server-side mail filtering via the Sieve language (RFC 5228). Scripts are stored per-user and executed on every incoming LMTP delivery. Script management is available via the ManageSieve protocol (RFC 5804) on port 4190.

## Supported extensions

`fileinto`, `reject`, `ereject`, `vacation`, `vacation-seconds`, `imap4flags`, `copy`, `envelope`, `body`, `date`, `index`, `regex`, `mailbox`, `special-use`, `editheader`, `variables`, `include`, `duplicate`, `ihave`, `notify`, `subaddress`

## Configuration

### `yarilo.yaml` — top-level `sieve:` section

| Key | Type | Default | Description |
|:----|:-----|:--------|:------------|
| `enabled` | bool | `false` | Activate Sieve execution on LMTP delivery |
| `max_script_size` | int | `65536` | Maximum compiled script size in bytes |
| `max_redirects` | int | `32` | Maximum `redirect` actions per message (RFC 5228 §6.2) |
| `vacation_enabled` | bool | `true` | Permit the `vacation` extension (RFC 5230) |
| `submission_host` | string | `""` | Upstream MTA for outbound mail (`host[:port]`, default port 25). Empty = redirect and vacation are silently dropped |
| `submission_ssl` | string | `"no"` | Transport security: `no` \| `smtps` \| `starttls` |
| `submission_timeout` | int | `30` | Connect and command timeout in seconds |
| `submission_auth_secret` | string | `""` | Name of a Kubernetes Secret containing `user` and `password` keys for SMTP AUTH. Leave empty for unauthenticated relay |

### Helm `values.yaml` — top-level `sieve:` section

| Key | Default | Description |
|:----|:--------|:------------|
| `sieve.enabled` | `false` | Enable Sieve |
| `sieve.maxScriptSize` | `65536` | Max script size (bytes) |
| `sieve.maxRedirects` | `32` | Max redirect actions per message |
| `sieve.vacationEnabled` | `true` | Enable vacation extension |
| `sieve.submissionHost` | `""` | Upstream MTA address (`host[:port]`) |
| `sieve.submissionSSL` | `"no"` | TLS mode: `no` / `smtps` / `starttls` |
| `sieve.submissionTimeout` | `30` | Timeout in seconds |
| `sieve.submissionAuthSecret` | `""` | Name of a Kubernetes Secret with `user` and `password` keys for SMTP AUTH. Leave empty for unauthenticated relay |

## Outbound mail — redirect and vacation

When `submission_host` is configured, yarilo dispatches outbound mail for:

- **`redirect`** — forwards the original message verbatim to the redirect address, preserving the original envelope-from (RFC 5228 §4.2).
- **`vacation`** — sends an RFC 5230 auto-reply to the original sender with a null envelope-from (`<>`) to prevent mail loops.

### Vacation dedup (RFC 5230 §4.5)

Yarilo enforces the per-sender reply interval specified in the vacation action (`:days` or `:seconds`, default 7 days). The last-sent timestamp is stored in the user's dict under `priv/sieve/vacation/<handle>/<sender>`. Dict drivers with TTL support (Redis) expire the entry automatically; other drivers use the stored timestamp for manual comparison.

Vacation replies are skipped when:
- The sender address is empty or `<>`.
- The message has a `List-Id` header or `Precedence: bulk/list/junk`.
- The message has `Auto-Submitted:` set to any value other than `no`.

### Notifications (RFC 5435 `enotify`)

The `notify` extension (RFC 5435) allows scripts to send notifications via an external method URI. Yarilo supports the `mailto:` method — the notification is sent as an email via the same `submission_host` as redirect and vacation.

```sieve
require ["notify"];
notify :message "New mail arrived" "mailto:admin@example.com";
```

The `mailto:` URI may include `subject` and `body` query parameters:

```
mailto:admin@example.com?subject=Alert&body=You+have+mail
```

- **From**: the delivery envelope recipient (your mailbox address).
- **Subject**: from the URI `subject=` parameter; defaults to `Notification` if absent.
- **Body**: `ActionNotify.Message` (`:message` argument in the script), or URI `body=` parameter if the script provides no message.
- **Envelope-from**: `<>` (null) to prevent mail loops.
- **Auto-Submitted**: `auto-generated`.

Methods other than `mailto:` (e.g. `xmpp:`, `sms:`) are logged at `WARN` level and silently dropped.

### Kubernetes secret for SMTP AUTH

Create the secret once:

```sh
kubectl create secret generic sieve-smtp-auth \
  --from-literal=user=relay_user \
  --from-literal=password=relay_password
```

Reference it in `values.yaml`:

```yaml
sieve:
  enabled: true
  submissionHost: "relay.example.com:587"
  submissionSSL: "starttls"
  submissionAuthSecret: "sieve-smtp-auth"
```

## Default script

On first delivery for a new user, yarilo seeds a default `yarilo.sieve` script:

```sieve
keep;
```

Operators can replace this via ManageSieve or by writing a script to the user's dict storage directly.
