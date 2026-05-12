# Yarilo — Implementation Plan

Yarilo is a production-grade IMAP/SMTP mail server written in Go,
targeting full feature parity with industry-standard open-source IMAP servers.

Internal protocols (yarilo-director, yarilo-auth, yarilo-dict, yarilo-admin, yarilo-stats)
are specified in [INTERNALS.md](INTERNALS.md).

---

## Architecture

Yarilo runs in one of three modes (`mode: proxy | director | backend`).
Single binary — three roles.

```
  Internet
     |
     | IMAP/IMAPs/POP3/POP3s/JMAP/SMTP
     v
+--------------------+     +--------------------+
|      PROXY         |     |      PROXY         |  ...N proxies
|                    |     |                    |
| - TLS termination  |     | - TLS termination  |
| - Auth (passdb)    |     | - Auth (passdb)    |
| - Route lookup     |     | - Route lookup     |
+--------+-----------+     +----------+---------+
         |  gRPC                      |
         |  "where is user X?"        |
         v                            v
+------------------------------------------------+
|                  DIRECTOR                      |
|                                                |
| - Consistent hashing: user@domain -> backend  |
| - Sticky sessions (one user = one backend)    |
| - Backend registry + health checks            |
| - Failover: reassign on node failure          |
+--------+----------+----------+----------------+
         |          |          |
         v          v          v
  +----------+ +----------+ +----------+
  | BACKEND  | | BACKEND  | | BACKEND  |  ...N backends
  |          | |          | |          |
  | IMAP     | | IMAP     | | IMAP     |
  | POP3     | | POP3     | | POP3     |
  | JMAP     | | JMAP     | | JMAP     |
  | Sieve    | | Sieve    | | Sieve    |
  | Quota    | | Quota    | | Quota    |
  | ACL      | | ACL      | | ACL      |
  +----+-----+ +----+-----+ +----+-----+
       |             |            |
       v             v            v
  +-----------+ +-----------+ +-----------+
  | Mailbox   | | Mailbox   | | Mailbox   |
  | Backend   | | Backend   | | Backend   |
  | +Index    | | +Index    | | +Index    |
  +-----------+ +-----------+ +-----------+
       |                            |
       v                            v
  [Maildir/dbox/mdbox]          [obox -> S3]
  [FileIndex/SQLite]            [Cassandra]
```

### Proxy
- Accepts client connections (IMAP/POP3/JMAP/SMTP)
- TLS termination, SNI per-domain
- Completes protocol handshake to extract username (see Protocol Proxying below)
- Authentication (passdb lookup)
- Queries Director: which backend to route user@domain to
- Forwards client IP + session metadata to backend
- Transparently proxies raw TCP stream to backend
- Stateless — scale horizontally without limits

### Director
- Single active node (or multiple with leader election via ring handshake)
- Consistent hashing ring: user@domain -> backend node (MD5, 100 vhosts/backend)
- Sticky sessions: all connections for one user go to the same backend
- Health checks to backend nodes (yarilo-director PING/PONG)
- Failover: on backend failure -> reassign users -> notify proxies
- yarilo-director TAB-delimited binary protocol (see INTERNALS.md §2) — proxy↔director and director↔director
- Protocol-aware routing: different backends can serve different protocols

### Backend
- Full IMAP/POP3/JMAP/SMTP server
- Sieve, Quota, ACL, FTS
- Access to MailboxBackend + IndexBackend
- Trusts proxy for pre-authenticated connections (via trusted network config)
- Not exposed to internet — internal cluster only
- Registers with Director on startup

---

### Proxy Headers + XCLIENT

Every protocol instance natively supports both directions:

**Inbound** — receiving real client IP from an upstream load balancer:

| Protocol | Mechanism |
|:---|:---|
| IMAP / POP3 / SMTP | PROXY protocol v1 + v2 (HAProxy) |
| JMAP (HTTP) | `X-Forwarded-For`, `X-Real-IP`, `Forwarded` (RFC 7239) |

**Outbound** — forwarding client metadata to backend:

| Protocol | Mechanism |
|:---|:---|
| IMAP | IMAP ID extension (`x-originating-ip`, `x-originating-port`, `x-session-id`) |
| POP3 | `XCLIENT ADDR=<ip> PORT=<port> SESSION=<id>` |
| SMTP inbound | `XCLIENT ADDR=<ip> PORT=<port> HELO=<helo>` (Postfix-compatible) |
| SMTP submission | `XCLIENT` + pre-authenticated session |
| JMAP | `X-Forwarded-For` + `X-Session-ID` HTTP headers |
| ManageSieve | IMAP-style ID forward |

Config per protocol instance:

```yaml
imap:
  proxy_protocol: true          # accept HAProxy PROXY v1/v2 from upstream LB
  xclient_trusted_nets:         # networks allowed to send XCLIENT to this instance
    - "10.0.0.0/8"
    - "172.16.0.0/12"
```

---

### Protocol Proxying

Each protocol requires the proxy to complete a partial handshake to identify
the user before the connection can be routed and forwarded.

| Protocol | Proxy completes | Forwarding mechanism |
|:---|:---|:---|
| IMAP | CAPABILITY, LOGIN/AUTHENTICATE | IMAP ID extension with `x-originating-ip`, `x-session-id`; backend accepts pre-auth |
| POP3 | USER + PASS | XCLIENT command: `XCLIENT ADDR=<ip> PORT=<port> SESSION=<id>` |
| SMTP inbound | EHLO | XCLIENT command (Postfix-compatible) |
| SMTP submission | EHLO + AUTH | XCLIENT command + pre-auth to backend |
| JMAP | HTTP auth header parse | HTTP `X-Forwarded-For` + `X-Session-ID` headers |
| ManageSieve | AUTHENTICATE | Capability + pre-auth forward |

**IMAP proxy flow:**
```
Client                  Proxy                   Backend
  |--- CONNECT -------->|                           |
  |<-- * OK Yarilo -----|                           |
  |--- LOGIN user pwd ->|                           |
  |    [auth passdb]    |                           |
  |    [ask director]   |--- CONNECT -------------->|
  |                     |<-- * OK Yarilo ----------|
  |                     |--- ID ("x-originating-ip" "1.2.3.4"
  |                     |        "x-session-id" "abc123") -->|
  |                     |--- AUTHENTICATE PLAIN ... ->|
  |<-- + OK ------------|<-- + OK ------------------|
  |=== transparent TCP proxy =========================|
```

**POP3 proxy flow:**
```
Client                  Proxy                   Backend
  |--- CONNECT -------->|                           |
  |<-- +OK Yarilo ------|                           |
  |--- USER alice ----->|                           |
  |--- PASS secret ---->|                           |
  |    [auth passdb]    |                           |
  |    [ask director]   |--- CONNECT -------------->|
  |                     |--- XCLIENT ADDR=1.2.3.4 ->|
  |                     |--- USER alice ----------->|
  |                     |--- PASS secret ---------->|
  |<-- +OK -------------|<-- +OK --------------------|
  |=== transparent TCP proxy =========================|
```

---

### Deployment modes

| Mode | Components | Use case |
|:---|:---|:---|
| Single node | proxy + director + backend in one process | dev / small |
| Multi-node | separate proxy / director / backend | production |
| Cloud | proxy + director + backend (obox + Cassandra) | large scale |

---

### Storage Layer

```
MailboxBackend (message storage format)
    |- Maildir    — local FS, one file per message
    |- dbox       — local FS, sdbox format
    |- mdbox      — local FS, multiple messages per file
    \- obox       — S3-compatible object storage

IndexBackend (IMAP index storage)
    |- FileIndex  — binary files alongside mailbox (single node)
    |- SQLite     — dev / small deployments
    \- Cassandra  — multi-node / large scale
```

Any combination: Maildir+FileIndex (single node),
mdbox+Cassandra (multi-node), obox+Cassandra (cloud).

---

## Directory Layout

```
yarilo/
|- cmd/
|  \- yarilo/             # single binary, mode via config
|- internal/
|  |- proxy/              # Proxy mode: TLS, auth, routing, stream splice
|  |- director/           # Director mode: hash ring, sticky sessions, health
|  |- backend/            # Backend mode: wiring all components
|  |- cluster/
|  |  |- proto/           # yarilo-director TAB-delimited protocol
|  |  \- ring/            # Consistent hashing (MD5, 100 vhosts) — build
|  |- proxyproto/         # HAProxy PROXY v1/v2 — go-proxyproto wrapper
|  |- xclient/            # XCLIENT (POP3/SMTP inbound+outbound) — build
|  |- hibernate/          # IMAP idle connection parking — build
|  |- anvil/              # Connection rate limiting + penalty — build
|  |- imap/               # IMAP server (go-imap/v2 + custom extensions)
|  |- pop3/               # POP3 server — build from scratch
|  |- jmap/               # JMAP Core + Mail (go-jmap + custom dispatch)
|  |  \- push/            # JMAP WebSocket push (RFC 8887) — build
|  |- smtp/               # SMTP inbound + outbound (go-smtp + XCLIENT)
|  |- lmtp/               # LMTP delivery (go-smtp + per-rcpt replies)
|  |- managesieve/        # ManageSieve (RFC 5804) — build from scratch
|  |- storage/
|  |  |- mailbox/
|  |  |  |- maildir/      # Maildir — build (INTERNALS.md §9)
|  |  |  |- dbox/         # sdbox — build (INTERNALS.md §9)
|  |  |  |- mdbox/        # mdbox — build (complex: map index, refcount)
|  |  |  \- obox/         # S3 via minio-go — build obox layer
|  |  \- index/
|  |     |- file/         # FileIndex binary — build (INTERNALS.md §8)
|  |     |- sqlite/       # SQLite index
|  |     \- cassandra/    # Cassandra index (gocql)
|  |- auth/
|  |  |- protocol/        # yarilo-auth TAB-delimited protocol — build
|  |  |- sql/             # SQL passdb (SQLite/MySQL/PostgreSQL)
|  |  \- oauth2/          # OAuth2/OIDC (XOAUTH2/OAUTHBEARER)
|  |- dict/               # yarilo-dict protocol + backends — build
|  |  |- redis/           # Redis backend
|  |  \- sqlite/          # SQLite dict backend
|  |- sieve/
|  |  |- parser/          # go-sieve AST (wrapper)
|  |  \- engine/          # Sieve execution engine — build
|  |- quota/              # Quota tracking + dict integration
|  |- acl/                # ACL + shared mailboxes
|  |- fts/
|  |  |- indexer/         # Indexer service protocol — build
|  |  |- sqlite/          # SQLite FTS5
|  |  \- elastic/         # Elasticsearch
|  |- dkim/               # go-msgauth wrapper
|  |- spf/                # blitiri/go-spf wrapper
|  |- dmarc/              # DMARC — build (no good library)
|  |- replication/        # dsync v3.5 — build (post-v1.0)
|  |- admin/              # Admin HTTP API
|  \- telemetry/          # Health + Prometheus (every instance)
|- pkg/
|  |- mailbox/            # Core interfaces + types
|  \- config/             # Config parsing
|- config/
|  \- yarilo.yaml         # Example config
|- doc/
|  |- presentation.md
|  \- presentation.pdf
\- docker/
   \- Dockerfile
```

---

## Core Interfaces

```go
// MailboxBackend — mailbox format abstraction
type MailboxBackend interface {
    Create(user, folder string) error
    Delete(user, folder string) error
    Save(user, folder string, msg []byte) (uid uint32, err error)
    Fetch(user, folder string, uid uint32) ([]byte, error)
    Remove(user, folder string, uid uint32) error
    List(user, folder string) ([]MessageMeta, error)
}

// IndexBackend — index storage abstraction
type IndexBackend interface {
    GetFolder(user, folder string) (*Folder, error)
    SaveFolder(user string, f *Folder) error
    AppendMessage(folderID uint64, m *MessageMeta) error
    UpdateFlags(folderID uint64, uid uint32, flags []string) error
    GetMessages(folderID uint64, seq SeqSet) ([]*MessageMeta, error)
    UpdateModSeq(folderID uint64) (uint64, error)
}
```

---

## Protocol Library Strategy

### Use as-is

| Library | Provides |
|:---|:---|
| `github.com/pires/go-proxyproto` | HAProxy PROXY protocol v1 + v2 (inbound + outbound) |
| `github.com/minio/minio-go/v7` | S3-compatible client for obox backend |
| `modernc.org/sqlite` | Pure-Go SQLite (no cgo) |
| `github.com/go-sql-driver/mysql` | MySQL driver |
| `github.com/jackc/pgx/v5` | PostgreSQL driver |
| `github.com/gocql/gocql` | Cassandra driver |
| `github.com/xdg-go/scram` | SCRAM-SHA-1 / SCRAM-SHA-256 |
| `github.com/emersion/go-msgauth` | DKIM sign + verify |
| `blitiri.com.ar/go/spf` | SPF verification |
| `github.com/emersion/go-milter` | Milter client (rspamd) |
| `github.com/prometheus/client_golang` | Prometheus metrics |
| `github.com/knadh/koanf/v2` | YAML config + env override |
| `log/slog` JSON handler | Structured logging (stdlib) |
| `crypto/tls` | TLS 1.2/1.3 + SNI via `GetConfigForClient` (stdlib) |

### Use as base, extend on top

| Library | Built-in | Must add on top |
|:---|:---|:---|
| `github.com/emersion/go-imap/v2` | IMAP server framework, IDLE, MOVE, CONDSTORE, UNSELECT, NAMESPACE, QUOTA, ACL, BINARY, SORT, THREAD | QRESYNC full (partial only), NOTIFY (RFC 5465), URLAUTH (RFC 4467), PREVIEW (RFC 8970), SPECIAL-USE, SASL-IR, LITERAL+ |
| `github.com/emersion/go-smtp` | SMTP + LMTP server, CHUNKING/BDAT, DSN, 8BITMIME, STARTTLS, per-recipient replies | XCLIENT (inbound + outbound), BURL |
| `git.sr.ht/~emersion/go-sieve` | Sieve AST parser (RFC 5228) — parses scripts into AST only | Full execution engine: fileinto, reject, vacation, imap4flags, copy, envelope, include, notify |
| `github.com/emersion/go-jmap` | JMAP Core types, partial RFC 8620 | HTTP method dispatch, server-side handler framework, WebSocket push (RFC 8887) |

### Build from scratch (no usable library exists)

| Component | Notes |
|:---|:---|
| POP3 server (RFC 1939) | No production-ready Go server library |
| ManageSieve server (RFC 5804) | Client libraries only, no server |
| XCLIENT protocol (POP3/SMTP) | ~100 lines, xtext-encoding, 512-byte line split |
| yarilo-director protocol | TAB-delimited ring protocol (INTERNALS.md §2) |
| yarilo-auth protocol | Passdb chain, auth cache, SASL dispatcher (INTERNALS.md §3) |
| yarilo-dict protocol | Dict abstraction + Redis/SQLite backends (INTERNALS.md §4) |
| yarilo-admin protocol | Admin multiplex streaming (INTERNALS.md §5) |
| yarilo-stats protocol | Prometheus event stream (INTERNALS.md §6) |
| Consistent hashing ring | MD5, 100 vhosts/backend, binary search — ~100 lines |
| Maildir backend | Filename format, flags encoding, dovecot-uidlist v3 (INTERNALS.md §9) |
| dbox backend | Magic bytes `\001\002`, metadata in file (INTERNALS.md §9) |
| mdbox backend | Multi-message files, map index, refcounting, purge |
| obox backend | Atomic write pattern on top of minio-go (INTERNALS.md §29) |
| FileIndex backend | Binary .index / .index.log / .index.cache (INTERNALS.md §8) |
| IMAP proxy logic | Pre-auth handshake, then transparent TCP stream splice |
| imap-hibernate | FD-passing, IMAP state serialization (INTERNALS.md §13) |
| Anvil rate limiting | Connection counting + penalty algorithm (INTERNALS.md §12) |
| dsync replication | Full wire protocol v3.5 (INTERNALS.md §17) — post-v1.0 |
| DMARC | No complete Go library available |
| Indexer service | Async FTS indexing daemon (INTERNALS.md §19) |

---

## Phase 1 — Core: single-node + IMAP + Maildir + FileIndex (target: 3-4 months)

> **Complexity note:** Maildir and FileIndex are the two hardest pieces in this phase.
> FileIndex is a custom binary format (.index + .index.log + .index.cache, see INTERNALS.md §8).
> Budget extra time for these before moving to Phase 2.

### Goals
- Proxy + Director + Backend interfaces defined from day one
- Single-node mode: all three in one process
- IMAP4rev1 + core extensions
- Maildir mailbox backend (local FS)
- FileIndex index backend (binary — built from scratch)
- Multi-tenant (user@domain)
- Auth: SQLite passdb
- TLS + SNI (per-domain certificates, `GetConfigForClient`)
- IMAP IDLE (RFC 2177)
- CONDSTORE (RFC 4551)
- `/healthz`, `/readyz`, `/metrics` on every instance

### IMAP Extensions (via go-imap/v2, extended where needed)
- `IDLE` — RFC 2177 (built-in)
- `MOVE` — RFC 6851 (built-in)
- `CONDSTORE` — RFC 4551 (built-in, partial QRESYNC deferred to Phase 5)
- `LITERAL+` — RFC 7888 (built on top)
- `SPECIAL-USE` — RFC 6154 (built on top)
- `UNSELECT` — RFC 3691 (built-in)
- `ID` — RFC 2971 (built-in)
- `NAMESPACE` — RFC 2342 (built-in)
- `SASL-IR` — RFC 4959 (built on top)
- `AUTH=PLAIN`, `AUTH=LOGIN`, `AUTH=SCRAM-SHA-256` (xdg-go/scram)

### Deliverables
- [ ] `go.mod` init, project skeleton
- [ ] `MailboxBackend` + `IndexBackend` + `Proxy` + `Director` + `Backend` interfaces
- [ ] `internal/cluster/ring` — consistent hashing (MD5, 100 vhosts, ~100 lines)
- [ ] `internal/cluster/proto` — yarilo-director TAB-delimited protocol (stub for single-node)
- [ ] `internal/auth/protocol` — yarilo-auth handshake + SASL dispatcher
- [ ] `internal/auth/sql` — SQLite passdb (modernc.org/sqlite)
- [ ] `internal/proxy` — single-node stub (direct connection to backend)
- [ ] `internal/director` — single-node stub (local routing)
- [ ] `internal/storage/mailbox/maildir` — Maildir (INTERNALS.md §9)
- [ ] `internal/storage/index/file` — FileIndex binary (INTERNALS.md §8)
- [ ] `internal/imap` — IMAP server (go-imap/v2 + extensions above)
- [ ] `internal/telemetry` — `/healthz`, `/readyz`, `/metrics`
- [ ] Config: `yarilo.yaml` with `mode: single`
- [ ] `cmd/yarilo` — single binary
- [ ] Docker: `golang:1.26-alpine` → `alpine:3.23`
- [ ] GitHub Actions CI: build + test (linux/amd64)
- [ ] README: config reference, deploy guide

---

## Phase 2 — dbox + mdbox backends (target: +1-2 months)

### Goals
- dbox (sdbox) mailbox backend — single-message format with metadata in file
- mdbox mailbox backend — multi-message files, highest density
- Flags stored in index only (like Dovecot dbox)
- Migration tool: Maildir -> dbox

### Deliverables
- [ ] `internal/storage/mailbox/dbox`
- [ ] `internal/storage/mailbox/mdbox`
- [ ] `cmd/yarilo-migrate` — Maildir -> dbox/mdbox converter
- [ ] README storage backends section

---

## Phase 3 — SMTP Inbound + Delivery (target: +1-2 months)

### Goals
- Receive inbound email (MX)
- LMTP local delivery to mailbox backend (per-recipient replies)
- XCLIENT support inbound (from upstream proxy) and outbound (to backend)
- DKIM verification + signing (go-msgauth)
- SPF verification (blitiri/go-spf)
- DMARC policy evaluation (custom — no complete library)
- Anti-spam via rspamd (milter — go-milter)
- SMTP submission (port 587, AUTH required)

### Deliverables
- [ ] `internal/smtp` — inbound SMTP server (go-smtp + XCLIENT extension)
- [ ] `internal/lmtp` — LMTP delivery (go-smtp LMTPSession, per-recipient replies)
- [ ] `internal/xclient` — XCLIENT inbound + outbound, xtext-encoding (build)
- [ ] `internal/dkim` — go-msgauth wrapper
- [ ] `internal/spf` — blitiri/go-spf wrapper
- [ ] `internal/dmarc` — DMARC policy engine (build from scratch)
- [ ] rspamd milter client (go-milter)
- [ ] README update

---

## Phase 4 — Auth Backends (target: +1 month)

### Goals
- SQL passdb/userdb: SQLite, MySQL, PostgreSQL (single driver, different DSN)
- OAuth2 / OIDC (XOAUTH2 SASL mechanism)
- Passdb chaining

### Deliverables
- [ ] `internal/auth/sql` — unified SQL backend supporting SQLite / MySQL / PostgreSQL
- [ ] `internal/auth/oauth2` — OIDC discovery, token introspection, XOAUTH2
- [ ] passdb chain config
- [ ] README auth section

### Backlog (out of scope for v1.0)
- LDAP passdb/userdb
- PAM passdb

---

## Phase 5 — Cluster Mode: Proxy / Director / Backend split (target: +2-3 months)

### Goals
- Full three-tier cluster: separate proxy, director, backend processes
- yarilo-director ring protocol fully implemented (INTERNALS.md §2)
- QRESYNC (RFC 7162) full implementation (builds on CONDSTORE from Phase 1)
- imap-hibernate: IMAP idle connection parking (INTERNALS.md §13)
- Anvil: connection rate limiting + penalty algorithm (INTERNALS.md §12)
- IMAP NOTIFY (RFC 5465)

### Deliverables
- [ ] `internal/proxy` — full proxy mode (TLS termination, auth, director query, stream splice)
- [ ] `internal/director` — full director mode (ring protocol, sticky sessions, health checks)
- [ ] `internal/cluster/proto` — complete yarilo-director wire protocol
- [ ] `internal/hibernate` — IMAP idle connection parking (FD-passing, state serialization)
- [ ] `internal/anvil` — connection rate limiting + penalty tracking
- [ ] QRESYNC extension for IMAP server (VANISHED response, known-uid-set)
- [ ] IMAP NOTIFY (RFC 5465) built on top of go-imap/v2
- [ ] Proxy loop prevention (LOGIN_PROXY_TTL=5, see INTERNALS.md §11)
- [ ] yarilo-dict: quota + ACL dict protocol (INTERNALS.md §4)
- [ ] README: cluster deploy guide

---

## Phase 7 — SQLite + Cassandra Index backends (target: +1-2 months)

### Goals
- SQLite IndexBackend — lightweight, single node, no deps
- Cassandra IndexBackend — multi-node, high availability
- Config switch between backends

### Deliverables
- [ ] `internal/storage/index/sqlite`
- [ ] `internal/storage/index/cassandra`
- [ ] Cassandra schema (keyspace, tables)
- [ ] README index backends section

---

## Phase 8 — obox backend (target: +1-2 months)

### Goals
- Object storage mailbox backend (S3-compatible)
- One S3 object per message, atomic write pattern (INTERNALS.md §29)
- Works with Cassandra index for multi-node
- Supports: AWS S3, MinIO, Ceph RGW, Cloudflare R2

### Object key structure
```
{domain}/{user}/{folder}/{uid}.eml
```

### Deliverables
- [ ] `internal/storage/mailbox/obox` — obox layer on top of minio-go
- [ ] Atomic write: write to temp key → rename (INTERNALS.md §29)
- [ ] obox + Cassandra wiring in config
- [ ] README obox section

---

## Phase 9 — Sieve + ManageSieve (target: +2-3 months)

> **Complexity note:** go-sieve provides AST parser only. The execution engine
> (fileinto, reject, vacation, imap4flags, copy, envelope) must be built from scratch.
> ManageSieve server (RFC 5804) has no usable Go library — build from scratch.

### Goals
- Sieve execution engine built on go-sieve AST parser
- Extensions: `fileinto`, `reject`, `vacation`, `imap4flags`, `copy`, `envelope`
- ManageSieve protocol server (RFC 5804, port 4190) — built from scratch
- Script storage via yarilo-dict

### Deliverables
- [ ] `internal/sieve/engine` — Sieve execution engine (build)
- [ ] `internal/managesieve` — ManageSieve server (build from scratch)
- [ ] Integration with LMTP delivery pipeline
- [ ] README update

---

## Phase 10 — Quota + ACL (target: +1-2 months)

### Goals
- Per-user + per-domain quotas (bytes + message count)
- Quota dict paths: `priv/quota/storage`, `priv/quota/messages`
- ACL: shared mailbox folders (RFC 4314)
- Shared namespace (`/shared/` in IMAP)
- Quota enforcement on APPEND + COPY

### IMAP Extensions
- `QUOTA` — RFC 9208
- `ACL` — RFC 4314
- `MYRIGHTS` — RFC 4314

### Deliverables
- [ ] `internal/quota` — quota enforcement + dict integration
- [ ] `internal/acl` — ACL rights (INTERNALS.md §10)
- [ ] Shared namespace in NAMESPACE response
- [ ] README update

---

## Phase 11 — POP3 (target: +1-2 months)

> **Complexity note:** No production-ready Go POP3 server library exists.
> Built entirely from scratch. Session lock, UIDL format, XCLIENT forwarding
> all custom (INTERNALS.md §24).

### Goals
- Full POP3 server (RFC 1939) — built from scratch
- POP3S (port 995, TLS) + STARTTLS (port 110)
- UIDL support (configurable keymask)
- Session lock (prevents concurrent access)
- XCLIENT forwarding from proxy
- Reuses same auth + mailbox + index backends

### Deliverables
- [ ] `internal/pop3` — POP3 server (build from scratch)
- [ ] POP3 proxy logic (USER+PASS → XCLIENT forward)
- [ ] README update

---

## Phase 12 — JMAP (target: +2-3 months)

> **Complexity note:** go-jmap provides types but no server dispatch framework.
> HTTP method routing, WebSocket push (RFC 8887), and session management built on top.

### Goals
- JMAP Core (RFC 8620) — HTTP/HTTPS API
- JMAP Mail (RFC 8621) — mailbox, email, thread operations
- JMAP over WebSocket (RFC 8887) — push notifications (build)
- Shared auth + mailbox + index backends with IMAP
- Port 443 (HTTPS) + WebSocket upgrade

### Deliverables
- [ ] `internal/jmap` — JMAP HTTP dispatch (go-jmap types + custom routing)
- [ ] `internal/jmap/push` — WebSocket push server (RFC 8887, build)
- [ ] README JMAP section

---

## Phase 13 — Full-Text Search + Indexer (target: +1-2 months)

### Goals
- SQLite FTS5 — zero deps, single node
- Elasticsearch backend — large deployments
- Indexer service protocol for async FTS (INTERNALS.md §19)
- Triggered on delivery, IMAP SEARCH extended

### Deliverables
- [ ] `internal/fts/indexer` — indexer service protocol (PREPEND/APPEND/OPTIMIZE)
- [ ] `internal/fts/sqlite` — SQLite FTS5
- [ ] `internal/fts/elastic` — Elasticsearch
- [ ] README update

---

## Phase 14 — Admin API + Provisioning (target: +1-2 months)

### Goals
- REST API: domain/user/mailbox management
- Prometheus metrics
- Per-domain config

### Endpoints
```
POST   /api/domains
DELETE /api/domains/:domain
POST   /api/users
DELETE /api/users/:email
PATCH  /api/users/:email/quota
GET    /api/users/:email/stats
GET    /metrics
```

### Deliverables
- [ ] `internal/admin`
- [ ] API key auth
- [ ] Prometheus: connections, messages/sec, storage used
- [ ] README API section

---

## Configuration Reference (yarilo.yaml)

```yaml
log_level: info

mode: single    # single | proxy | director | backend

storage:
  mailbox:
    backend: maildir        # maildir | dbox | mdbox | obox
    root: /var/mail/yarilo  # for maildir/dbox/mdbox
    obox:                   # only when backend: obox
      endpoint: "s3.amazonaws.com"
      bucket: "yarilo-mail"
      region: "eu-central-1"
      access_key: ""
      secret_key: ""

  index:
    backend: file                    # file | sqlite | cassandra
    path: /var/lib/yarilo/index      # for file | sqlite
    cassandra:                       # only when backend: cassandra
      hosts:
        - "cassandra-1:9042"
        - "cassandra-2:9042"
      keyspace: yarilo
      username: ""
      password: ""

auth:
  backends:
    - type: sql
      dsn: "/var/lib/yarilo/users.db"               # SQLite
      # dsn: "mysql://user:pass@localhost/yarilo"    # MySQL
      # dsn: "postgres://user:pass@localhost/yarilo" # PostgreSQL
    - type: oauth2
      issuer: "https://accounts.example.com"
      client_id: ""
      client_secret: ""

imap:
  listeners:
    - addr: ":993"
      tls: true
    - addr: ":143"
      starttls: true
  idle_timeout: 30m

smtp:
  inbound:
    addr: ":25"
    hostname: "mail.example.com"
  submission:
    addr: ":587"
    require_auth: true
  dkim:
    selector: "default"
    private_key: "/etc/yarilo/dkim/private.pem"
  rspamd:
    url: "http://localhost:11333"

pop3:
  listeners:
    - addr: ":995"
      tls: true
    - addr: ":110"
      starttls: true

managesieve:
  addr: ":4190"

jmap:
  addr: ":443"
  tls: true

telemetry:
  addr: "0.0.0.0:9090"         # every instance (proxy/director/backend)
  health_path:  /healthz        # GET -> 200 OK / 503
  ready_path:   /readyz         # GET -> 200 OK when ready to serve traffic
  metrics_path: /metrics        # Prometheus exposition format

admin:
  addr: "127.0.0.1:8080"
  api_key: ""

tls:
  domains:
    - domain: "example.com"
      cert: "/etc/yarilo/certs/example.com/fullchain.pem"
      key:  "/etc/yarilo/certs/example.com/privkey.pem"
```

---

## Environment Variables

| Variable | Default | Description |
|:---|:---|:---|
| `YARILO_CONFIG` | `/etc/yarilo/yarilo.yaml` | Config file path |
| `LOG_LEVEL` | `info` | debug / info / warn / error |
| `S3_ACCESS_KEY` | --- | obox S3 access key |
| `S3_SECRET_KEY` | --- | obox S3 secret key |
| `CASSANDRA_USERNAME` | --- | Cassandra username |
| `CASSANDRA_PASSWORD` | --- | Cassandra password |
| `ADMIN_API_KEY` | --- | Admin API key |

---

## Backend Combinations

| Use case | MailboxBackend | IndexBackend |
|:---|:---|:---|
| Dev / local | Maildir | SQLite |
| Single node production | mdbox | FileIndex |
| Multi-node (shared FS) | mdbox | Cassandra |
| Cloud / object storage | obox | Cassandra |

---

## Feature Checklist

### Protocols

| Protocol | Port | TLS | RFC |
|:---|:---|:---|:---|
| IMAP | 143 | STARTTLS | RFC 3501 / 9051 |
| IMAPs | 993 | TLS | RFC 3501 / 9051 |
| POP3 | 110 | STARTTLS | RFC 1939 |
| POP3s | 995 | TLS | RFC 1939 |
| LMTP | 24 | optional | RFC 2033 |
| SMTP inbound | 25 | STARTTLS | RFC 5321 |
| SMTP submission | 587 | STARTTLS | RFC 6409 |
| ManageSieve | 4190 | STARTTLS | RFC 5804 |
| JMAP | 443 | TLS | RFC 8620/8621 |
| JMAP WebSocket | 443 | TLS+WS | RFC 8887 |

- [ ] IMAP4rev1 (RFC 3501)
- [ ] IMAP4rev2 (RFC 9051)
- [ ] POP3 (RFC 1939)
- [ ] LMTP (RFC 2033)
- [ ] SMTP inbound (RFC 5321)
- [ ] SMTP submission (RFC 6409)
- [ ] Sieve filtering (RFC 5228)
- [ ] ManageSieve (RFC 5804)
- [ ] JMAP Core (RFC 8620)
- [ ] JMAP Mail (RFC 8621)
- [ ] JMAP WebSocket push (RFC 8887)

### IMAP Extensions
- [ ] IDLE (RFC 2177) *(go-imap/v2 built-in)*
- [ ] MOVE (RFC 6851) *(go-imap/v2 built-in)*
- [ ] CONDSTORE (RFC 4551) *(go-imap/v2 built-in)*
- [ ] QRESYNC (RFC 7162) *(partial in go-imap/v2, extend)*
- [ ] LITERAL+ (RFC 7888) *(build on top of go-imap/v2)*
- [ ] SPECIAL-USE (RFC 6154) *(build on top)*
- [ ] NAMESPACE (RFC 2342) *(go-imap/v2 built-in)*
- [ ] ACL (RFC 4314) *(go-imap/v2 built-in)*
- [ ] QUOTA (RFC 9208) *(go-imap/v2 built-in)*
- [ ] ID (RFC 2971) *(go-imap/v2 built-in)*
- [ ] UNSELECT (RFC 3691) *(go-imap/v2 built-in)*
- [ ] NOTIFY (RFC 5465) *(build on top)*
- [ ] PREVIEW (RFC 8970) *(build on top)*
- [ ] SASL-IR (RFC 4959) *(build on top)*
- [ ] URLAUTH (RFC 4467) *(build on top)*
- [ ] BINARY (RFC 3516) *(go-imap/v2 built-in)*

### Mailbox Backends
- [ ] Maildir
- [ ] dbox (sdbox)
- [ ] mdbox
- [ ] obox (S3-compatible)

### Index Backends
- [ ] FileIndex (binary)
- [ ] SQLite
- [ ] Cassandra

### Auth
- [ ] SQL passdb/userdb (SQLite / MySQL / PostgreSQL)
- [ ] OAuth2/OIDC (XOAUTH2)
- [ ] Passdb chaining
- [ ] LDAP passdb/userdb *(backlog)*
- [ ] PAM passdb *(backlog)*

### Filtering
- [ ] Sieve (RFC 5228)
- [ ] ManageSieve (RFC 5804)
- [ ] fileinto, reject, vacation, imap4flags

### Security
- [ ] TLS 1.2 + 1.3
- [ ] SNI (per-domain certificates)
- [ ] DKIM sign + verify
- [ ] SPF verification
- [ ] DMARC policy
- [ ] Anti-spam (rspamd milter)

### Cluster
- [ ] Proxy mode
- [ ] Director mode (consistent hashing, sticky sessions)
- [ ] Backend mode
- [ ] Single-node mode (all three in one process)
- [ ] yarilo-director TAB-delimited protocol (proxy↔director, director ring)
- [ ] yarilo-auth protocol (SASL, passdb chain, auth cache)
- [ ] yarilo-dict protocol (quota, ACL, Sieve script storage)
- [ ] yarilo-admin protocol (admin commands, multiplex streaming)
- [ ] yarilo-stats protocol (event stream, Prometheus)
- [ ] Director ring handshake + leader election
- [ ] Backend health checks (PING/PONG) + failover
- [ ] Per-protocol proxy instances (independent config + tuning)
- [ ] Tag-based backend routing (route by domain, user, tag)
- [ ] Proxy loop prevention (LOGIN_PROXY_TTL=5)

### Proxy Headers + XCLIENT
- [ ] HAProxy PROXY protocol v1 inbound
- [ ] HAProxy PROXY protocol v2 inbound
- [ ] XCLIENT inbound (POP3, SMTP)
- [ ] XCLIENT outbound to backend (POP3, SMTP)
- [ ] IMAP ID extension outbound (`x-originating-ip`, `x-session-id`)
- [ ] HTTP `X-Forwarded-For` / `Forwarded` inbound (JMAP)
- [ ] HTTP `X-Forwarded-For` / `X-Session-ID` outbound (JMAP)
- [ ] `xclient_trusted_nets` per protocol instance

### Observability
- [ ] `/healthz` on every instance
- [ ] `/readyz` on every instance
- [ ] `/metrics` Prometheus on every instance

### Features
- [ ] Multi-tenant (user@domain)
- [ ] Per-user + per-domain quotas
- [ ] ACL / shared mailboxes
- [ ] Shared namespace
- [ ] imap-hibernate (idle connection parking)
- [ ] Anvil connection rate limiting + penalty
- [ ] FTS (SQLite FTS5)
- [ ] FTS (Elasticsearch)
- [ ] Indexer service (async FTS)
- [ ] Admin REST API

### Replication (post-v1.0)
- [ ] dsync protocol v3.5 (INTERNALS.md §17)
- [ ] Replication daemon (INTERNALS.md §18)
- [ ] Backup send / backup recv modes

---

## Release Milestones

| Milestone | Content | Target |
|:---|:---|:---|
| v0.1.0 | Phase 1 — IMAP + Maildir + FileIndex + single-node | Month 4 |
| v0.2.0 | Phase 2 — dbox + mdbox | Month 6 |
| v0.3.0 | Phase 3 — SMTP + Delivery + XCLIENT | Month 8 |
| v0.4.0 | Phase 4 — Auth backends | Month 9 |
| v0.5.0 | Phase 5 — Cluster mode (proxy/director/backend, hibernate, anvil) | Month 12 |
| v0.6.0 | Phase 6 — IMAP NOTIFY + QRESYNC full | Month 13 |
| v0.7.0 | Phase 7 — SQLite + Cassandra index | Month 15 |
| v0.8.0 | Phase 8 — obox (S3) | Month 17 |
| v0.9.0 | Phase 9 — Sieve engine + ManageSieve | Month 20 |
| v0.10.0 | Phase 10 — Quota + ACL | Month 22 |
| v0.11.0 | Phase 11 — POP3 | Month 24 |
| v0.12.0 | Phase 12 — JMAP | Month 27 |
| v0.13.0 | Phase 13 — FTS + Indexer | Month 29 |
| v1.0.0 | Phase 14 — Admin API + hardening | Month 31 |
| v1.1.0 | Replication (dsync) | post-v1.0 |
