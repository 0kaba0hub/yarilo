# yarilo

[![CI](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml/badge.svg)](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-blue)](https://github.com/0kaba0hub/yarilo)
[![License: GPL v3](https://img.shields.io/badge/license-GPLv3-blue.svg)](LICENSE)
[![Status: pre-alpha](https://img.shields.io/badge/status-pre--alpha-orange)](PLAN.md)

Production-grade IMAP/SMTP/JMAP mail server written in Go.
Three-tier cluster (proxy → director → backend), pluggable storage (Maildir / dbox / mdbox / S3), Sieve filtering, full Dovecot 2.3 protocol compatibility.

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
mode: backend   # proxy | director | backend

imap:
  listen: ":993"
  listen_plain: ":143"        # STARTTLS
  tls_cert: /etc/ssl/yarilo/cert.pem
  tls_key:  /etc/ssl/yarilo/key.pem
  proxy_protocol: false       # set true when behind HAProxy

smtp:
  listen_mx:     ":25"
  listen_submit: ":587"
  hostname: mail.example.com
  max_message_size: 41943040  # 40 MB
  tls_cert: /etc/ssl/yarilo/cert.pem
  tls_key:  /etc/ssl/yarilo/key.pem
  proxy_protocol: false       # set true when behind HAProxy
  xclient: false              # advertise and handle XCLIENT on MX (trusted relay infrastructure)

  # Optional external milters (e.g. rspamd). Checked before internal SPF/DKIM/DMARC.
  # socket formats: unix:/path/to/milter.sock | /path/to/milter.sock | tcp:host:port
  milters:
    - socket: unix:/run/rspamd/milter.sock  # local unix socket
      timeout: 30             # seconds
    # - socket: tcp:127.0.0.1:11332         # remote milter over TCP
    #   timeout: 10

spf:
  enabled: true

dmarc:
  enabled: true

dkim:
  verify: true                # verify on inbound (MX)
  sign:   true                # sign on outbound (submission)
  selector: mail

  # Static key backend: domain → PEM file path
  keys:
    backend: static
    static:
      example.com: /etc/yarilo/dkim/example.com.pem

  # Dynamic key backend: fetch from SQL DB (use backend: dynamic)
  # keys:
  #   backend: dynamic
  #   dynamic:
  #     driver: postgres
  #     dsn: "${DKIM_DB_URL}"   # ${ENV_VAR} substitution supported
  #     query: "SELECT private_key FROM dkim_keys WHERE domain = $1"
  #     cache_ttl: 300          # seconds

auth:
  passdb:
    - driver: postgres
      dsn: "${DB_URL}"          # ${ENV_VAR} substitution supported

storage:
  mailbox: maildir
  maildir_root: /var/mail/vhosts

log:
  level: info   # debug | info | warn | error
```

```sh
yarilo -config yarilo.yaml
```

Set `LOG_LEVEL=debug` to enable verbose protocol tracing without restarting.

### SMTP inbound pipeline (port 25)

```
connect → external milters → SPF check → DKIM verify → DMARC evaluate → LMTP local delivery
```

If a milter rejects the message a `550 5.7.1` response is returned. Milter unavailability is fail-open (message continues).

### SMTP submission pipeline (port 587)

```
connect → AUTH PLAIN → external milters → DKIM sign → relay (phase 4)
```

Submission requires AUTH PLAIN. DKIM signing uses the key for the sender domain. The signed message is logged; relay to the MTA queue is implemented in phase 4.

### DKIM key backends

| `keys.backend` | Source | Config |
|:---|:---|:---|
| `static` | PEM files on disk | `keys.static.<domain>: /path/to/key.pem` |
| `dynamic` | SQL database | `keys.dynamic.{driver,dsn,query,cache_ttl}` |

`${ENV_VAR}` in any DSN or TLS path is expanded at startup — no secrets in config files.

### HAProxy PROXY protocol

Set `proxy_protocol: true` on `imap` and/or `smtp` to extract the real client IP from the HAProxy `PROXY` header. This applies to all listeners on that server (MX + submission for SMTP; TLS + STARTTLS for IMAP).

### Mailbox backend selection

| Value | Format | Description |
|:---|:---|:---|
| `maildir` | Maildir | one file per message, `cur/` + `new/` + `tmp/` |
| `dbox` | sdbox | one file per message in dbox wire format (GUID + metadata embedded) |
| `mdbox` | mdbox | multiple messages per `m.<id>` file, higher density |

```yaml
storage:
  mailbox: dbox          # maildir | dbox | mdbox
  maildir_root: /var/mail/vhosts
```

### Migration from Maildir

```sh
yarilo-migrate \
  --from /var/mail/vhosts \
  --to   /var/mail/dbox \
  --format dbox          # or mdbox
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
