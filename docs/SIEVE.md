# Sieve Mail Filtering

Yarilo implements server-side mail filtering via the Sieve language (RFC 5228). Scripts are stored per-user and executed on every incoming LMTP delivery. Script management is available via the ManageSieve protocol (RFC 5804) on port 4190.

## Supported extensions

`fileinto`, `reject`, `ereject`, `vacation`, `vacation-seconds`, `imap4flags`, `copy`, `envelope`, `body`, `date`, `index`, `regex`, `mailbox`, `special-use`, `fcc`, `editheader`, `variables`, `include`, `duplicate`, `ihave`, `enotify`, `subaddress`, `spamtest`, `spamtestplus`, `virustest`, `foreverypart`, `mime`, `extracttext`, `replace`, `enclose`, `vnd.yarilo.debug`, `vnd.yarilo.environment`, `vnd.yarilo.pipe`, `vnd.yarilo.filter`, `vnd.yarilo.execute`

`foreverypart` / `mime` / `extracttext` (RFC 5703) provide per-MIME-part
processing: `foreverypart { … }` walks the parts depth-first (with `break
[:name]`), `header`/`exists :mime` inspect the current part's headers with the
`:type` / `:subtype` / `:contenttype` / `:param` / `:anychild` selectors, and
`extracttext [:first N] [MODIFIER] var` captures the current part's UTF-8 text
(with the RFC 5229 value modifiers) into a variable. `replace [:mime] [:subject
s] [:from s] <text>` rewrites the current part (or the whole message outside a
loop); `enclose [:subject s] [:headers list] <text>` wraps the message in a
multipart/mixed cover plus the original as `message/rfc822`. A rewritten message
is delivered in place of the original. Octet-exact preservation of a pre-existing
`multipart/signed` part across a `replace` is a known limitation.

`spamtest` / `spamtestplus` / `virustest` (RFC 5235) are backed by a configured status header (see `sieve_spamtest_status_header` / `sieve_virustest_status_header` below); with no header configured the tests report "not scanned". `extlists`, `mboxmetadata`, and `servermetadata` are **not** advertised yet — their backing data sources are not wired.

## Configuration

### `yarilo.yaml` — top-level `sieve:` section

| Key | Type | Default | Description |
|:----|:-----|:--------|:------------|
| `sieve_enabled` | bool | `false` | Activate Sieve execution on LMTP delivery |
| `sieve_max_script_size` | int | `65536` | Maximum compiled script size in bytes |
| `sieve_max_redirects` | int | `32` | Maximum `redirect` actions per message (RFC 5228 §6.2) |
| `sieve_max_actions` | int | `32` | Maximum total actions per script (`fileinto`/`redirect`/`keep`/...). `0` = unlimited. Guards runaway scripts |
| `sieve_vacation_enabled` | bool | `true` | Permit the `vacation` extension (RFC 5230) |
| `sieve_spamtest_status_header` | string | `""` | Header carrying the spam score for `spamtest` (RFC 5235), e.g. `X-Spam-Score`. Empty = test reports "not scanned" |
| `sieve_spamtest_max_value` | float | `10` | Raw header value mapped to the top of the 0..10 (0..100 with `:percent`) scale |
| `sieve_virustest_status_header` | string | `""` | Header carrying the virus verdict for `virustest` (RFC 5235). Empty = "not scanned" |
| `sieve_virustest_max_value` | float | `5` | Raw header value mapped to the top of the 0..5 virus scale |
| `sieve_submission_host` | string | `""` | Upstream MTA for outbound mail (`host[:port]`, default port 25). Empty = redirect and vacation are silently dropped |
| `sieve_submission_ssl` | string | `"no"` | Transport security: `no` \| `smtps` \| `starttls` |
| `sieve_submission_timeout` | int | `30` | Connect and command timeout in seconds |
| `sieve_submission_auth_secret` | string | `""` | Name of a Kubernetes Secret containing `user` and `password` keys for SMTP AUTH. Leave empty for unauthenticated relay |

All keys keep the `sieve_` prefix even under the `sieve:` section, matching the config koanf tags.

### Helm `values.yaml` — top-level `sieve:` section

The `values.yaml` keys are identical to the `yarilo.yaml` keys above (same `sieve_` prefix):

| Key | Default | Description |
|:----|:--------|:------------|
| `sieve.sieve_enabled` | `false` | Enable Sieve |
| `sieve.sieve_max_script_size` | `65536` | Max script size (bytes) |
| `sieve.sieve_max_redirects` | `32` | Max redirect actions per message |
| `sieve.sieve_max_actions` | `32` | Max total actions per script (`0` = unlimited) |
| `sieve.sieve_vacation_enabled` | `true` | Enable vacation extension |
| `sieve.sieve_spamtest_status_header` | `""` | Spam-score header for `spamtest` (empty = unbacked) |
| `sieve.sieve_spamtest_max_value` | `10` | Top of the spam-score scale |
| `sieve.sieve_virustest_status_header` | `""` | Virus-verdict header for `virustest` (empty = unbacked) |
| `sieve.sieve_virustest_max_value` | `5` | Top of the virus-score scale |
| `sieve.sieve_submission_host` | `""` | Upstream MTA address (`host[:port]`) |
| `sieve.sieve_submission_ssl` | `"no"` | TLS mode: `no` / `smtps` / `starttls` |
| `sieve.sieve_submission_timeout` | `30` | Timeout in seconds |
| `sieve.sieve_submission_auth_secret` | `""` | Name of a Kubernetes Secret with `user` and `password` keys for SMTP AUTH. Leave empty for unauthenticated relay |

## Outbound mail — redirect and vacation

When `sieve_submission_host` is configured, yarilo dispatches outbound mail for:

- **`redirect`** — forwards the original message verbatim to the redirect address, preserving the original envelope-from (RFC 5228 §4.2).
- **`vacation`** — sends an RFC 5230 auto-reply to the original sender with a null envelope-from (`<>`) to prevent mail loops.

### Vacation dedup (RFC 5230 §4.5)

Yarilo enforces the per-sender reply interval specified in the vacation action (`:days` or `:seconds`, default 7 days). The last-sent timestamp is stored in the user's dict under `priv/sieve/vacation/<handle>/<sender>`. Dict drivers with TTL support (Redis) expire the entry automatically; other drivers use the stored timestamp for manual comparison.

Vacation replies are skipped when:
- The sender address is empty or `<>`.
- The message has a `List-Id` header or `Precedence: bulk/list/junk`.
- The message has `Auto-Submitted:` set to any value other than `no`.

### Duplicate dedup (RFC 7352)

The `duplicate` test is backed by the dict named `sieve_duplicate` in the
`dicts:` config, keyed by `priv/<user>/sieve/duplicate/<handle>/<sha256(id)>`
with the action's TTL. Configure it with `driver: redis` so the dedup window is
shared **across pods** in a multi-pod backend; with no `sieve_duplicate` dict
(or a `memory` driver) the state is per-process — fine for single-node but it
lets a message re-delivered to a different pod slip through.

### Notifications (RFC 5435 `enotify`)

The `notify` extension (RFC 5435) allows scripts to send notifications via an external method URI. Yarilo supports the `mailto:` method — the notification is sent as an email via the same `sieve_submission_host` as redirect and vacation.

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
  sieve_enabled: true
  sieve_submission_host: "relay.example.com:587"
  sieve_submission_ssl: "starttls"
  sieve_submission_auth_secret: "sieve-smtp-auth"
```

## Yarilo-specific extensions

Yarilo ships four proprietary Sieve extensions under the `vnd.yarilo.*` namespace. They must be listed in the `require` statement of any script that uses them.

---

### `vnd.yarilo.debug` — script-level debug logging

Appends timestamped messages to `.yarilo.sieve.log` in the user's home directory. Intended for troubleshooting script logic without touching system logs.

```sieve
require ["vnd.yarilo.debug"];
debug_log "fileinto triggered for ${subject}";
```

The log file is created on first write with mode `0600`. Each line is `<RFC 3339 UTC timestamp>  <message>`. No configuration required.

---

### `vnd.yarilo.environment` — operator-defined environment items

Exposes delivery-time variables to scripts via the standard `environment` test. Built-in items:

| Item name | Value |
|:----------|:------|
| `vnd.yarilo.username` | Full login name (`user@domain`) |
| `vnd.yarilo.default-mailbox` | Always `INBOX` |
| `vnd.yarilo.config.<key>` | Operator-defined string from `sieve.sieve_environment` |

```sieve
require ["environment"];
if environment :is "vnd.yarilo.username" "alice@example.com" {
    fileinto "VIP";
}
```

Operator config in `yarilo.yaml`:

```yaml
sieve:
  sieve_environment:
    tenant: "acme"
    region: "eu-west-1"
```

Exposed as `vnd.yarilo.config.tenant` and `vnd.yarilo.config.region`.

---

### `vnd.yarilo.pipe` — pipe message to an external program

Feeds the full RFC 5322 message to an external program. The program receives no output — exit code determines success. Useful for archiving, indexing, or side-effect triggering.

```sieve
require ["vnd.yarilo.pipe"];
pipe "archive-mail" ["--folder" "inbox"];
```

**Program resolution** (tried in order):
1. Unix socket `<sieve_pipe_socket_dir>/<name>` — if the path is a socket file, yarilo connects and writes the message; socket output is discarded.
2. Executable `<sieve_pipe_bin_dir>/<name>` — launched as a subprocess.

World-writable executables are refused.

**Environment variables** injected into subprocesses:

| Variable | Value |
|:---------|:------|
| `USER` | Delivery recipient login name |
| `SENDER` | Envelope sender address |
| `RECIPIENT` | Envelope recipient address |
| `HOME` | Process home directory |
| `HOST` | Hostname |

**Configuration** (`yarilo.yaml` / `values.yaml`):

| Key | Default | Description |
|:----|:--------|:------------|
| `sieve_pipe_bin_dir` | `/usr/lib/yarilo/sieve-pipe` | Directory of allowed pipe executables |
| `sieve_pipe_socket_dir` | `sieve-pipe` | Directory of allowed pipe sockets (searched first) |
| `sieve_pipe_exec_timeout` | `10` | Subprocess timeout in seconds |
| `sieve_pipe_input_eol` | `crlf` | Line endings written to stdin: `crlf` or `lf` |

---

### `vnd.yarilo.filter` — rewrite message through an external program

Like `pipe`, but the program's stdout replaces the message body. If the program exits non-zero or produces no output, the original message is passed through unchanged.

```sieve
require ["vnd.yarilo.filter"];
if filter "add-disclaimer" [] {
    fileinto "Filtered";
}
```

The `filter` action returns `true` if the program exited 0 and produced output. The same program resolution and environment variable rules as `vnd.yarilo.pipe` apply.

**Configuration** (`yarilo.yaml` / `values.yaml`):

| Key | Default | Description |
|:----|:--------|:------------|
| `sieve_filter_bin_dir` | `/usr/lib/yarilo/sieve-filter` | Directory of allowed filter executables |
| `sieve_filter_socket_dir` | `sieve-filter` | Directory of allowed filter sockets (searched first) |
| `sieve_filter_exec_timeout` | `10` | Subprocess timeout in seconds |
| `sieve_filter_input_eol` | `crlf` | Line endings written to stdin: `crlf` or `lf` |

---

### `vnd.yarilo.execute` — run a program and capture its output

Runs a program with optional stdin and makes its stdout available to the script. Unlike `pipe`/`filter`, the program does not receive the full message unless the script explicitly passes content. Exit code is exposed as a boolean result.

```sieve
require ["vnd.yarilo.execute", "variables"];
if execute :input "check" "quota-check" [] {
    # exit 0 — quota OK
} else {
    reject "Quota exceeded";
}
```

The `execute` action returns `true` on exit 0. For Unix socket targets, non-empty output implies success (sockets have no exit code).

The same program resolution and environment variable rules as `vnd.yarilo.pipe` apply.

**Configuration** (`yarilo.yaml` / `values.yaml`):

| Key | Default | Description |
|:----|:--------|:------------|
| `sieve_execute_bin_dir` | `/usr/lib/yarilo/sieve-execute` | Directory of allowed execute programs |
| `sieve_execute_socket_dir` | `sieve-execute` | Directory of allowed execute sockets (searched first) |
| `sieve_execute_exec_timeout` | `10` | Subprocess timeout in seconds |
| `sieve_execute_input_eol` | `crlf` | Line endings written to stdin: `crlf` or `lf` |

---

## Default script

On first delivery for a new user, yarilo seeds a default `yarilo.sieve` script:

```sieve
keep;
```

Operators can replace this via ManageSieve or by writing a script to the user's dict storage directly.
