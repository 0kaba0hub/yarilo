# yarilo

<table><tr>
<td><img src="https://raw.githubusercontent.com/0kaba0hub/yarilo/main/docs/icon.svg" width="180" alt="yarilo logo"/></td>
<td>

Production-grade IMAP/SMTP/JMAP mail server written in Go.
Three-tier cluster (proxy → director → backend), pluggable storage (Maildir / dbox / mdbox / S3), Sieve filtering, full Dovecot 2.3 protocol compatibility.

[![CI](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml/badge.svg)](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-blue)](https://github.com/0kaba0hub/yarilo)
[![License: GPL v3](https://img.shields.io/badge/license-GPLv3-blue.svg)](LICENSE)
[![Status: pre-alpha](https://img.shields.io/badge/status-pre--alpha-orange)](PLAN.md)

</td>
</tr></table>

---

## Architecture

Yarilo is a single binary that runs in one of three roles (`mode: proxy | director | backend`).

```
  Internet
     |
     | IMAP / IMAPs / SUBMISSION / SMTP / POP3 / JMAP
     v
+------------------+     +------------------+
|     PROXY        |     |     PROXY        |  ...N
|                  |     |                  |
| TLS termination  |     | TLS termination  |
| Auth (passdb)    |     | Auth (passdb)    |
| Route lookup     |     | Route lookup     |
+--------+---------+     +--------+---------+
         |  TAB-delimited protocol           |
         v                                   v
+------------------------------------------------+
|                  DIRECTOR                      |
|                                                |
| Consistent hashing: user@domain -> backend     |
| Sticky sessions (one user = one backend)       |
| Backend registry + health checks              |
| Failover: reassign on node failure             |
+--------+----------+----------+----------------+
         |          |          |
         v          v          v
  +----------+ +----------+ +----------+
  | BACKEND  | | BACKEND  | | BACKEND  |  ...N
  |          | |          | |          |
  | IMAP     | | IMAP     | | IMAP     |
  | POP3     | | POP3     | | POP3     |
  | JMAP     | | JMAP     | | JMAP     |
  | SMTP/LDA | | SMTP/LDA | | SMTP/LDA |
  | Sieve    | | Sieve    | | Sieve    |
  | Quota    | | Quota    | | Quota    |
  | ACL      | | ACL      | | ACL      |
  +----+-----+ +----+-----+ +----+-----+
       |             |            |
       v             v            v
  +-----------+ +-----------+ +-----------+
  | Mailbox   | | Mailbox   | | Mailbox   |
  | + Index   | | + Index   | | + Index   |
  +-----------+ +-----------+ +-----------+
       |                            |
       v                            v
  [Maildir/dbox/mdbox]          [obox -> S3]
  [FileIndex / SQLite]          [Cassandra]
```

**Proxy** — accepts client connections, terminates TLS (SNI per-domain), authenticates via passdb, queries Director for routing, forwards the raw TCP stream to the target backend. Stateless; scale horizontally without limits.

**Director** — consistent-hashing ring (MD5, 100 vhosts/backend). One user always lands on the same backend. Detects failures and reassigns. All intra-cluster traffic uses the TAB-delimited yarilo-director protocol (see [INTERNALS.md](INTERNALS.md) §2).

**Backend** — full mail server: IMAP, POP3, JMAP, SMTP/LMTP, ManageSieve, Sieve execution engine, Quota, ACL, FTS. Speaks natively to the mailbox + index layer.

---

## Protocol support

| Protocol | Standard | Extensions |
|:---|:---|:---|
| IMAP4rev2 | RFC 9051 | IDLE, MOVE, CONDSTORE, UNSELECT, NAMESPACE, QUOTA, ACL, BINARY, UIDPLUS, SORT, THREAD, ESEARCH, NOTIFY, QRESYNC, URLAUTH, SPECIAL-USE |
| SMTP / Submission | RFC 5321, RFC 6409 | STARTTLS, AUTH PLAIN/LOGIN/SCRAM, SIZE, PIPELINING, CHUNKING/BDAT, DSN, XCLIENT |
| LMTP | RFC 2033 | per-recipient replies, quota checks |
| POP3 | RFC 1939 | STLS, UIDL, CAPA, XCLIENT |
| JMAP | RFC 8620, RFC 8621 | HTTP dispatch, WebSocket push (RFC 8887) |
| ManageSieve | RFC 5804 | full Sieve script management |
| Sieve | RFC 5228 | fileinto, reject, vacation, notify, include, variables, date, relational, imap4flags, editheader, extlists, vnd.dovecot.* |
| SASL | — | PLAIN, LOGIN, GSSAPI, SCRAM-SHA-256, XOAUTH2, OAUTHBEARER |
| TLS | — | SNI, per-domain context reload |

---

## Storage

| Layer | Backend | Notes |
|:---|:---|:---|
| Mailbox | Maildir | one file per message, local FS |
| Mailbox | dbox (sdbox) | single-file dbox, local FS |
| Mailbox | mdbox | multi-message dbox, local FS |
| Mailbox | obox | S3-compatible object storage via minio-go |
| Index | FileIndex | custom binary format (`.index` / `.index.log` / `.index.cache`) alongside mailbox |
| Index | SQLite | dev / small deployments |
| Index | Cassandra | multi-node / large scale |
| FTS | built-in tokenizer | snowball stemmer + ICU normalizer |
| FTS | Solr | HTTP XML bridge |

---

## Deployment modes

| Mode | Process layout | Use case |
|:---|:---|:---|
| Single node | proxy + director + backend in one process | dev / small server |
| Multi-node | separate proxy / director / backend | production |
| Cloud | proxy + director + backend (obox + Cassandra) | large scale |

---

## Cluster components

| Component | Role |
|:---|:---|
| yarilo-director | consistent hashing, sticky sessions, failover |
| yarilo-auth | passdb chain, auth cache, SASL dispatch |
| yarilo-dict | Redis / SQLite key-value abstraction |
| yarilo-admin | control socket (kick, reload, stats) |
| yarilo-stats | per-user / per-domain connection metrics |
| yarilo-anvil | connection rate limiting + penalty algorithm |
| imap-hibernate | idle IMAP connection parking via FD-passing |
| yarilo-indexer | background FTS indexing service |

All intra-cluster protocols are TAB-delimited text with LF termination and a version handshake.
Full wire-format specification: [INTERNALS.md](INTERNALS.md).

---

## Quick start (single-node)

```yaml
# yarilo.yaml
mode: single   # proxy | director | backend | single

general:
  ssl:
    tls_cert: /etc/ssl/yarilo/cert.pem
    tls_key:  /etc/ssl/yarilo/key.pem
  haproxy:
    trusted_nets: ["127.0.0.1/32", "10.0.0.0/8"]
    timeout: 3
  xclient:
    trusted_nets: ["127.0.0.1/32", "10.0.0.0/8"]
  limits:
    mail_max_userip_connections: 10

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

protocol:
  imap:
    imap_idle_notify_interval: 120
    imap_max_line_length: 65536
    imap_id_send: "name *"
  smtp:
    hostname: mail.example.com
    max_message_size: 41943040
    recipient_delimiter: "+"
    milters:
      - socket: unix:/run/rspamd/milter.sock
        timeout: 30

spf:
  enabled: true
dmarc:
  enabled: true
dkim:
  verify: true
  sign: true
  selector: mail
  keys:
    backend: static
    static:
      example.com: /etc/yarilo/dkim/example.com.pem

auth:
  passdb:
    - driver: postgres
      dsn: "${DB_URL}"

storage:
  mailbox: maildir
  maildir_root: /var/mail/vhosts

log:
  level: info
```

```sh
yarilo -config yarilo.yaml
```

Set `LOG_LEVEL=debug` to enable verbose protocol tracing without restarting.

---

## Configuration reference

### `general` — shared infrastructure

#### `general.ssl`

Shared TLS certificate used by all TLS-enabled listeners. Individual services can override with a per-service `ssl:` block.

| Key | Default | Description |
|:---|:---|:---|
| `tls_cert` | — | Path to PEM certificate (or chain). `${ENV_VAR}` expanded at startup. |
| `tls_key` | — | Path to PEM private key. |
| `tls_alt_cert` | — | Optional second certificate (e.g. ECDSA) for dual-cert SNI. |
| `tls_alt_key` | — | Private key for `tls_alt_cert`. |
| `tls_min_version` | `TLS1.2` | Minimum TLS version: `TLS1.2` \| `TLS1.3`. |
| `prefer_server_ciphers` | `false` | Use server cipher-suite preference order. |

#### `general.haproxy`

HAProxy PROXY protocol v1/v2. When enabled on a service the real client IP is extracted from the `PROXY` header instead of the TCP source address. Only connections from `trusted_nets` are accepted; others ignore the header.

| Key | Default | Description |
|:---|:---|:---|
| `timeout` | `3` | Seconds to wait for the PROXY header before closing the connection. |
| `trusted_nets` | `["127.0.0.1/32","10.0.0.0/8"]` | CIDRs allowed to send PROXY headers. |

#### `general.xclient`

SMTP XCLIENT pre-auth command (trusted relay passes real client info). Per-service `xclient_protocol: true` must also be set.

| Key | Default | Description |
|:---|:---|:---|
| `trusted_nets` | `["127.0.0.1/32","10.0.0.0/8"]` | CIDRs allowed to send XCLIENT. |

#### `general.limits`

| Key | Default | Description |
|:---|:---|:---|
| `mail_max_userip_connections` | `10` | Max simultaneous connections per user+IP pair across IMAP and POP3. `0` = unlimited. |

---

### `services` — per-listener

Each listener is a named key under `services`. A missing key or `enabled: false` means the listener is not started. Common fields:

| Key | Default | Description |
|:---|:---|:---|
| `enabled` | `false` | Start this listener. |
| `port` | see table | TCP port to bind. |
| `ssl_mode` | — | `ssl` = wrap in TLS immediately; `starttls` = plain with STARTTLS upgrade; `no` = plain only. |
| `haproxy_protocol` | `false` | Enable HAProxy PROXY protocol on this listener (uses `general.haproxy` settings). |
| `xclient_protocol` | `false` | Enable XCLIENT on this listener (uses `general.xclient` settings). |
| `disable_plaintext_auth` | `false` | Reject AUTH/USER commands unless the connection is TLS-protected. |

Listeners and their defaults:

| Service key | Port | ssl_mode | Notes |
|:---|:---|:---|:---|
| `imaps` | `993` | `ssl` | IMAP over TLS (implicit). |
| `imap` | `143` | `starttls` | IMAP with STARTTLS. |
| `smtp` | `25` | `no` | MX inbound. No AUTH. |
| `submission` | `587` | `starttls` | Outbound submission. AUTH required. |
| `submissions` | `465` | `ssl` | Outbound submission, implicit TLS (port 465). |
| `pop3` | `110` | `starttls` | POP3 with STARTTLS. |
| `pop3s` | `995` | `ssl` | POP3 over TLS (implicit). |

HAProxy and XClient settings (`timeout`, `trusted_nets`) are always taken from `general.haproxy` / `general.xclient` — the per-service flags only enable/disable the feature.

---

### `protocol.imap`

Behaviour settings for all IMAP listeners (both `imaps` and `imap`).

| Key | Default | Description |
|:---|:---|:---|
| `imap_idle_notify_interval` | `120` | Seconds between unsolicited EXISTS responses during IDLE. `0` = disabled. |
| `imap_max_line_length` | `65536` | Max IMAP command line length in bytes. `0` = unlimited. |
| `imap_id_send` | `name *` | Space-separated key-value pairs sent in ID response (RFC 2971). `*` = server defaults. Empty string = ID extension disabled. |
| `login_greeting` | `""` | Custom server greeting replacing the default `IMAP server ready`. Empty = default. |
| `imap_logout_format` | `""` | Log line format at session end. Variables: `%{deleted}` `%{expunged}` `%{fetch_hdr_count}` `%{fetch_hdr_bytes}` `%{fetch_body_count}` `%{fetch_body_bytes}`. Empty = no stats line. |

---

### `protocol.smtp`

Behaviour settings shared across `smtp`, `submission`, and `submissions` listeners.

| Key | Default | Description |
|:---|:---|:---|
| `hostname` | system hostname | EHLO/HELO banner and Message-ID domain. |
| `max_message_size` | `41943040` | Max accepted message size in bytes (40 MiB). |
| `max_line_length` | `4096` | Max SMTP command/data line length in bytes. |
| `recipient_delimiter` | `+` | Subaddress separator: `user+tag@domain` → `user@domain`. Empty = disabled. |
| `milters[].socket` | — | Milter socket: `unix:/path` or `tcp:host:port`. Checked before internal SPF/DKIM/DMARC. |
| `milters[].timeout` | `30` | Milter response timeout in seconds. |

### SMTP inbound pipeline (port 25)

```
connect → external milters → SPF check → DKIM verify → DMARC evaluate → LMTP local delivery
```

A milter `550` rejection returns `550 5.7.1` to the sender. Milter unavailability is fail-open.

### SMTP submission pipeline (port 587 / 465)

```
connect → AUTH PLAIN → external milters → DKIM sign → relay (phase 4)
```

Submission requires AUTH PLAIN. DKIM signing uses the key for the sender domain.

---

### `protocol.pop3`

Behaviour settings for all POP3 listeners (`pop3` and `pop3s`).

| Key | Default | Description |
|:---|:---|:---|
| `pop3_no_flag_updates` | `false` | `false` = set `\Seen` on RETR'd messages at QUIT (Dovecot default). `true` = no flag changes. |
| `pop3_reuse_xuidl` | `false` | Use `X-UIDL` header for UIDL values (migration from Courier / qmail / cPanel). |
| `pop3_uidl_format` | `%u.%v` | UIDL format string. Variables: `%u` UID, `%v` UIDValidity, `%f` filename, `%g` GUID, `%m` MD5(filename). Dovecot compat: `%08Xu%08Xv`. |
| `pop3_uidl_duplicates` | `rename` | `allow` = emit duplicate UIDLs as-is. `rename` = append `-N` suffix to keep UIDLs unique. |
| `pop3_enable_last` | `false` | Advertise and handle the `LAST` command (RFC 1460). |
| `pop3_delete_type` | `expunge` | `expunge` = remove message from disk at QUIT. `flag` = set `pop3_deleted_flag` (soft delete). |
| `pop3_deleted_flag` | `""` | IMAP flag set when `pop3_delete_type: flag`. Example: `$POP3Deleted`. |

---

### DKIM key backends

| `keys.backend` | Source | Config |
|:---|:---|:---|
| `static` | PEM files on disk | `keys.static.<domain>: /path/to/key.pem` |
| `dynamic` | SQL database | `keys.dynamic.{driver,dsn,query,cache_ttl}` |

```yaml
# Static
dkim:
  keys:
    backend: static
    static:
      example.com: /etc/yarilo/dkim/example.com.pem

# Dynamic (SQL — supports sqlite | mysql | postgres)
dkim:
  keys:
    backend: dynamic
    dynamic:
      driver: postgres
      dsn: "${DKIM_DB_URL}"
      query: "SELECT private_key FROM dkim_keys WHERE domain = $1"
      cache_ttl: 300   # seconds
```

`${ENV_VAR}` in any `dsn` or TLS path is expanded at startup — no secrets in config files.

---

### Mailbox backend selection

| Value | Format | Description |
|:---|:---|:---|
| `maildir` | Maildir | one file per message, `cur/` + `new/` + `tmp/` |
| `dbox` | sdbox | one file per message in dbox wire format (GUID + metadata embedded) |
| `mdbox` | mdbox | multiple messages per `m.<id>` file, higher density |

```yaml
storage:
  mailbox: dbox
  maildir_root: /var/mail/vhosts
```

### Migration from Maildir

```sh
yarilo-migrate \
  --from /var/mail/vhosts \
  --to   /var/mail/dbox \
  --format dbox   # or mdbox
```

Use `--dry-run` to preview without writing.

---

## Documentation

| Document | Contents |
|:---|:---|
| [PLAN.md](PLAN.md) | Implementation plan, phases, library strategy, timelines |
| [INTERNALS.md](INTERNALS.md) | Wire-format specs for all internal protocols (33 sections) |

---

## License

GNU General Public License v3.0 — see [LICENSE](LICENSE).
