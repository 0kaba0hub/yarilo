# yarilo

<table><tr>
<td><img src="https://raw.githubusercontent.com/0kaba0hub/yarilo/main/docs/icon.svg" width="180" alt="yarilo logo"/></td>
<td>

Production-grade IMAP/Submission/LMTP/JMAP mail server written in Go.
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
     | IMAP / IMAPs / SUBMISSION / POP3 / JMAP
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
  | Sub/LMTP | | Sub/LMTP | | Sub/LMTP |
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

**Backend** — full mail server: IMAP, POP3, JMAP, Submission, LMTP, ManageSieve, Sieve execution engine, Quota, ACL, FTS. Speaks natively to the mailbox + index layer.

---

## Protocol support

| Protocol | Standard | Extensions |
|:---|:---|:---|
| IMAP4rev2 | RFC 9051 | IDLE, MOVE, CONDSTORE, UNSELECT, NAMESPACE, QUOTA, ACL, BINARY, UIDPLUS, SORT, THREAD, ESEARCH, NOTIFY, QRESYNC, URLAUTH, SPECIAL-USE |
| Submission | RFC 6409 | STARTTLS, AUTH PLAIN, SIZE, PIPELINING, relay to upstream MTA, ALPN matching, configurable `Received:` header |
| LMTP | RFC 2033 | per-recipient status codes, proxy/director mode, HAProxy, XCLIENT (incl. `DESTADDR`/`DESTPORT`), STARTTLS, Delivered-To header, per-user concurrency limit, client workarounds |
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

| Component | Role | Status |
|:---|:---|:---|
| yarilo-auth | passdb chain, auth cache, SASL dispatch — TCP+mTLS | ✅ v1.0.0 |
| yarilo-director | consistent hashing, sticky sessions, failover | planned |
| yarilo-anvil | connection rate limiting + penalty algorithm | planned |
| yarilo-locks | cross-process write coordination — embedded (standalone) / remote Redis-backed (backend per tag) | planned |
| yarilo-dict | Redis / SQLite key-value abstraction | planned |
| yarilo-admin | control socket (kick, reload, stats) | planned |
| yarilo-stats | per-user / per-domain connection metrics | planned |
| imap-hibernate | idle IMAP connection parking via FD-passing | planned |
| yarilo-indexer | background FTS indexing service | planned |

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
  submission:
    enabled: true
    port: 587
    ssl_mode: starttls
    disable_plaintext_auth: true
  lmtp:
    enabled: true
    port: 24
    ssl_mode: no
    xclient_protocol: true

protocol:
  imap:
    imap_idle_notify_interval: 120
    imap_max_line_length: 65536
    imap_id_send: "name *"
  submission:
    hostname: mail.example.com
    max_message_size: 41943040
    recipient_delimiter: "+"

auth:
  passdb:
    - driver: postgres
      dsn: "${DB_URL}"

# yarilo-auth standalone process (multi-process mode).
# Listen address and mTLS cert paths used by the yarilo-auth binary.
auth_service:
  listen: ":9100"
  mtls:
    cert: /etc/yarilo/tls/tls.crt
    key:  /etc/yarilo/tls/tls.key
    ca:   /etc/yarilo/tls/ca.crt
  shutdown:
    session_grace_period: 30  # seconds to drain sessions on SIGTERM
    kill_timeout: 5           # seconds before forced exit

storage:
  mailbox: maildir
  maildir_root: /var/mail/vhosts
  mail_home_template: "%d/%n"   # %d=domain %n=local %u=full email

log:
  level: info
```

```sh
yarilo -config yarilo.yaml
```

Set `LOG_LEVEL=debug` to enable verbose protocol tracing without restarting.

---

## Mailbox backends

| Value | Format | Description |
|:---|:---|:---|
| `maildir` | Maildir | one file per message, `cur/` + `new/` + `tmp/` |
| `dbox` | sdbox | one file per message in dbox wire format (GUID + metadata embedded) |
| `mdbox` | mdbox | multiple messages per `m.<id>` file, higher density |

```sh
yarilo-migrate \
  --from /var/mail/vhosts \
  --to   /var/mail/dbox \
  --format dbox   # or mdbox; --dry-run to preview
```

---

## Documentation

| Document | Contents |
|:---|:---|
| [PLAN.md](PLAN.md) | Implementation plan, phases, library strategy, timelines |
| [INTERNALS.md](INTERNALS.md) | Wire-format specs for all internal protocols (33 sections) |
| [docs/GENERAL.md](docs/GENERAL.md) | `general`: shared SSL, HAProxy, XClient, connection limits |
| [docs/SERVICES.md](docs/SERVICES.md) | `services`: per-listener config for all 7 listeners |
| [docs/IMAP.md](docs/IMAP.md) | `protocol.imap`: IDLE interval, line length, ID, logout format |
| [docs/SUBMISSION.md](docs/SUBMISSION.md) | `protocol.submission`: hostname, size limits, relay |
| [docs/LMTP.md](docs/LMTP.md) | `protocol.lmtp`: local delivery, proxy/director mode, HAProxy, XCLIENT, TLS, headers |
| [docs/POP3.md](docs/POP3.md) | `protocol.pop3`: UIDL format, soft-delete, migration |
| [docs/JMAP.md](docs/JMAP.md) | JMAP (planned, Phase 5) |
| [INSTALL.md](INSTALL.md) | End-to-end Kubernetes deploy with cert-manager + Let's Encrypt |
| [docs/AUTH.md](docs/AUTH.md) | `auth.passdb`: SQL backends (SQLite/MySQL/Postgres), password schemes |
| [docs/SMOKE.md](docs/SMOKE.md) | End-to-end smoke test: AUTH → LMTP → IMAPS/POP3S read |
| [docs/DIRECTOR.md](docs/DIRECTOR.md) | `director_service`: ring config, peers, HAProxy, XCLIENT, mTLS, Helm values |
| [docs/MONITOR.md](docs/MONITOR.md) | `yarilo-monitor` sidecar: backend health probes, tag credentials, Helm values, Prometheus metrics |
| [docs/ADMIN-API.md](docs/ADMIN-API.md) | HTTP admin API: auth, IP whitelist, all endpoints with request/response examples |
| [docs/YARILO-ADMIN.md](docs/YARILO-ADMIN.md) | `yarilo-admin` CLI: all subsystems (`director`, `dict`), commands, flags, env vars, usage examples |
| [docs/DICT.md](docs/DICT.md) | `pkg/dict` KV-store abstraction (Dovecot lib-dict analog): drivers (file/redis/sql/memory/fail), YAML schema, `yarilo-admin dict` CLI |

---

## License

GNU General Public License v3.0 — see [LICENSE](LICENSE).
