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

Submission requires AUTH PLAIN. DKIM signing uses the private key for the sender domain.

---

## DKIM

### Top-level keys

| Key | Default | Description |
|:---|:---|:---|
| `dkim.verify` | `false` | Verify DKIM signatures on inbound MX mail. Result is passed to the DMARC evaluator. |
| `dkim.sign` | `false` | Sign outbound submission mail with the sender domain's private key. |
| `dkim.selector` | `mail` | DKIM selector (e.g. `mail` → DNS record `mail._domainkey.example.com`). |
| `dkim.sign_headers` | `From,To,Subject,Date,Message-ID,Content-Type` | Headers included in the signature. |
| `dkim.oversign_headers` | `From` | Headers oversigned (signed one extra time) to prevent injection attacks. |

### Key backends

**Static** — PEM files on disk:

```yaml
dkim:
  sign: true
  selector: mail
  keys:
    backend: static
    static:
      example.com: /etc/yarilo/dkim/example.com.pem
      other.org:   /etc/yarilo/dkim/other.org.pem
```

**Dynamic** — SQL database (keys fetched at signing time, cached for `cache_ttl` seconds):

```yaml
dkim:
  sign: true
  selector: mail
  keys:
    backend: dynamic
    dynamic:
      driver: postgres          # sqlite | mysql | postgres
      dsn: "${DKIM_DB_URL}"     # ${ENV_VAR} substitution supported
      query: "SELECT private_key FROM dkim_keys WHERE domain = $1"
      cache_ttl: 300
```

The query must return a single column with the RSA/Ed25519 private key in PEM format.

### DNS record

```
mail._domainkey.example.com. IN TXT "v=DKIM1; k=rsa; p=<base64-pubkey>"
```

```sh
# Generate key pair
openssl genrsa -out example.com.pem 2048
openssl rsa -in example.com.pem -pubout | grep -v '^-' | tr -d '\n'
```

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
