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
|  |- proxy/              # Proxy mode: TLS, auth, routing, connection proxy
|  |- director/           # Director mode: hash ring, sticky sessions, health
|  |- backend/            # Backend mode: wiring all components
|  |- cluster/
|  |  |- proto/           # yarilo-director TAB-delimited protocol (proxy<->director<->backend)
|  |  \- ring/            # Consistent hashing ring (MD5, 100 vhosts)
|  |- proxyproto/         # HAProxy PROXY protocol v1/v2 (inbound + outbound)
|  |- xclient/            # XCLIENT command (POP3/SMTP inbound + outbound)
|  |- imap/               # IMAP protocol (go-imap/v2)
|  |- pop3/               # POP3 protocol
|  |- jmap/               # JMAP Core + Mail (go-jmap)
|  |  \- push/            # JMAP WebSocket push (RFC 8887)
|  |- smtp/               # SMTP inbound + outbound
|  |- lmtp/               # LMTP delivery
|  |- managesieve/        # ManageSieve protocol (RFC 5804)
|  |- storage/
|  |  |- mailbox/
|  |  |  |- maildir/      # Maildir
|  |  |  |- dbox/         # sdbox
|  |  |  |- mdbox/        # mdbox
|  |  |  \- obox/         # S3-compatible
|  |  \- index/
|  |     |- file/         # FileIndex (binary)
|  |     |- sqlite/       # SQLite
|  |     \- cassandra/    # Cassandra
|  |- auth/
|  |  |- sql/             # SQLite / MySQL / PostgreSQL
|  |  \- oauth2/          # OAuth2/OIDC (XOAUTH2)
|  |- sieve/              # Sieve engine (RFC 5228)
|  |- quota/              # Quota tracking + enforcement
|  |- acl/                # ACL + shared mailboxes
|  |- fts/
|  |  |- sqlite/          # SQLite FTS5
|  |  \- elastic/         # Elasticsearch
|  |- dkim/               # DKIM sign + verify
|  |- spf/                # SPF
|  |- dmarc/              # DMARC
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

| Layer | Library | Why |
|:---|:---|:---|
| IMAP wire format | `github.com/emersion/go-imap/v2` | RFC 3501/9051 + extensions, MIT |
| SMTP wire format | `github.com/emersion/go-smtp` | RFC 5321, same author |
| Sieve engine | `github.com/emersion/go-sieve` | RFC 5228, same ecosystem |
| JMAP | `github.com/emersion/go-jmap` | RFC 8620/8621/8887, same author |
| S3 client (obox) | `github.com/minio/minio-go/v7` | S3-compatible, production |
| SQLite | `modernc.org/sqlite` | pure Go, no cgo |
| MySQL | `github.com/go-sql-driver/mysql` | standard driver |
| PostgreSQL | `github.com/jackc/pgx/v5` | native driver |
| Cassandra | `github.com/gocql/gocql` | mature driver |
| TLS / SNI | stdlib `crypto/tls` | per-domain cert loading |
| Logging | `log/slog` JSON handler | standard, structured |
| Config | `github.com/knadh/koanf/v2` | YAML + env override |

---

## Phase 1 — Core: single-node + IMAP + Maildir + FileIndex (target: 2-3 months)

### Goals
- Proxy + Director + Backend interfaces defined from day one
- Single-node mode: all three in one process
- IMAP4rev1 + core extensions
- Maildir mailbox backend (local FS)
- FileIndex index backend
- Multi-tenant (user@domain)
- Auth: SQLite passdb
- TLS + SNI (per-domain certificates)
- IMAP IDLE (RFC 2177)
- CONDSTORE / QRESYNC (RFC 7162)
- `/healthz`, `/readyz`, `/metrics` on every instance

### IMAP Extensions
- `IDLE` — RFC 2177
- `MOVE` — RFC 6851
- `CONDSTORE` — RFC 4551
- `QRESYNC` — RFC 7162
- `LITERAL+` — RFC 7888
- `SPECIAL-USE` — RFC 6154
- `UNSELECT` — RFC 3691
- `ID` — RFC 2971
- `NAMESPACE` — RFC 2342
- `SASL-IR` — RFC 4959
- `AUTH=PLAIN`, `AUTH=LOGIN`, `AUTH=SCRAM-SHA-256`

### Deliverables
- [ ] `go.mod` init, project skeleton
- [ ] `MailboxBackend` + `IndexBackend` + `Proxy` + `Director` + `Backend` interfaces
- [ ] `internal/cluster/ring` — consistent hashing (MD5, 100 vhosts)
- [ ] `internal/cluster/proto` — yarilo-director TAB-delimited protocol
- [ ] `internal/auth` — yarilo-auth protocol (VERSION handshake, SASL mechanisms)
- [ ] `internal/proxy` — single-node stub (direct connection to backend)
- [ ] `internal/director` — single-node stub (local routing)
- [ ] `internal/storage/mailbox/maildir`
- [ ] `internal/storage/index/file`
- [ ] `internal/imap` — IMAP server (go-imap/v2)
- [ ] `internal/auth/sql` — SQLite passdb
- [ ] `internal/telemetry` — `/healthz`, `/readyz`, `/metrics`
- [ ] Config: `yarilo.yaml` with `mode: single`
- [ ] `cmd/yarilo` — single binary
- [ ] Docker: `golang:1.26-alpine` -> `alpine:3.23`
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
- LMTP local delivery to mailbox backend
- DKIM verification + signing
- SPF verification
- DMARC policy evaluation
- Anti-spam via rspamd (milter protocol)
- SMTP submission (port 587, AUTH required)

### Deliverables
- [ ] `internal/smtp` — inbound SMTP server (go-smtp)
- [ ] `internal/lmtp` — LMTP delivery
- [ ] `internal/dkim`
- [ ] `internal/spf`
- [ ] `internal/dmarc`
- [ ] rspamd milter client
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

## Phase 5 — SQLite + Cassandra Index backends (target: +1-2 months)

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

## Phase 6 — obox backend (target: +1-2 months)

### Goals
- Object storage mailbox backend (S3-compatible)
- One S3 object per message
- Works with Cassandra index for multi-node
- Supports: AWS S3, MinIO, Ceph RGW, Cloudflare R2

### Object key structure
```
{domain}/{user}/{folder}/{uid}.eml
```

### Deliverables
- [ ] `internal/storage/mailbox/obox`
- [ ] S3 client integration (minio-go)
- [ ] obox + Cassandra wiring in config
- [ ] README obox section

---

## Phase 7 — Sieve + ManageSieve (target: +1-2 months)

### Goals
- Sieve script execution on delivery (RFC 5228)
- ManageSieve protocol (RFC 5804, port 4190)
- Extensions: `fileinto`, `reject`, `vacation`, `imap4flags`, `copy`, `envelope`
- Script storage on local FS (one file per user)

### Deliverables
- [ ] `internal/sieve`
- [ ] `internal/managesieve`
- [ ] Integration with LMTP pipeline
- [ ] README update

---

## Phase 8 — Quota + ACL (target: +1-2 months)

### Goals
- Per-user + per-domain quotas (bytes + message count)
- ACL: shared mailbox folders (RFC 4314)
- Shared namespace (`/shared/` in IMAP)
- Quota enforcement on APPEND + COPY

### IMAP Extensions
- `QUOTA` — RFC 9208
- `ACL` — RFC 4314
- `MYRIGHTS` — RFC 4314

### Deliverables
- [ ] `internal/quota`
- [ ] `internal/acl`
- [ ] Shared namespace in NAMESPACE response
- [ ] README update

---

## Phase 9 — POP3 (target: +1 month)

### Goals
- Full POP3 server (RFC 1939)
- POP3S (port 995, TLS)
- STARTTLS (port 110)
- UIDL support
- Reuses same auth + mailbox + index backends

### Deliverables
- [ ] `internal/pop3`
- [ ] README update

---

## Phase 10 — JMAP (target: +2-3 months)

### Goals
- JMAP Core (RFC 8620) — HTTP/HTTPS API
- JMAP Mail (RFC 8621) — mailbox, email, thread operations
- JMAP over WebSocket (RFC 8887) — push notifications
- Shared auth + mailbox + index backends with IMAP
- Port 443 (HTTPS) + WebSocket upgrade

### Deliverables
- [ ] `internal/jmap` — JMAP Core + Mail handler (go-jmap)
- [ ] `internal/jmap/push` — WebSocket push (RFC 8887)
- [ ] README JMAP section

---

## Phase 11 — Full-Text Search (target: +1-2 months)

### Goals
- SQLite FTS5 — zero deps, single node
- Elasticsearch backend — large deployments
- Triggered on delivery, IMAP SEARCH extended

### Deliverables
- [ ] `internal/fts/sqlite` — FTS5
- [ ] `internal/fts/elastic` — Elasticsearch
- [ ] README update

---

## Phase 12 — Admin API + Provisioning (target: +1-2 months)

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
- [ ] IDLE (RFC 2177)
- [ ] MOVE (RFC 6851)
- [ ] CONDSTORE (RFC 4551)
- [ ] QRESYNC (RFC 7162)
- [ ] LITERAL+ (RFC 7888)
- [ ] SPECIAL-USE (RFC 6154)
- [ ] NAMESPACE (RFC 2342)
- [ ] ACL (RFC 4314)
- [ ] QUOTA (RFC 9208)
- [ ] ID (RFC 2971)
- [ ] UNSELECT (RFC 3691)
- [ ] PREVIEW (RFC 8970)
- [ ] SASL-IR (RFC 4959)

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
- [ ] FTS (SQLite FTS5)
- [ ] FTS (Elasticsearch)
- [ ] Admin REST API

---

## Release Milestones

| Milestone | Content | Target |
|:---|:---|:---|
| v0.1.0 | Phase 1 — IMAP + Maildir + FileIndex + single-node | Month 3 |
| v0.2.0 | Phase 2 — dbox + mdbox | Month 5 |
| v0.3.0 | Phase 3 — SMTP + Delivery | Month 7 |
| v0.4.0 | Phase 4 — Auth backends | Month 8 |
| v0.5.0 | Phase 5 — SQLite + Cassandra index | Month 10 |
| v0.6.0 | Phase 6 — obox (S3) | Month 12 |
| v0.7.0 | Phase 7 — Sieve + ManageSieve | Month 14 |
| v0.8.0 | Phase 8 — Quota + ACL | Month 16 |
| v0.9.0 | Phase 9 — POP3 | Month 18 |
| v0.10.0 | Phase 10 — JMAP | Month 21 |
| v0.11.0 | Phase 11 — FTS | Month 23 |
| v1.0.0 | Phase 12 — Admin API + hardening | Month 25 |
