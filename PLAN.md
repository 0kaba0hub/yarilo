# Yarilo — Implementation Plan

Yarilo is a production-grade IMAP/SMTP mail server written in Go,
targeting full feature parity with industry-standard open-source IMAP servers.

---

## Architecture

Yarilo запускається в одному з трьох режимів (`mode: proxy | director | backend`).
Один бінарник — три ролі.

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
| - Sticky sessions (один юзер = один backend)  |
| - Backend registry + health checks            |
| - Failover: reassign при падінні ноди         |
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
  [Maildir/dbox/mdbox]          [obox → S3]
  [FileIndex/SQLite]            [Cassandra]
```

### Proxy
- Приймає з'єднання клієнтів (IMAP/POP3/JMAP/SMTP)
- TLS termination, SNI per-domain
- Аутентифікація (passdb lookup)
- Запитує Director: "на який backend роутити user@domain?"
- Проксує з'єднання до backend (IMAP proxy protocol)
- Stateless — можна додавати без обмежень

### Director
- Один (або кілька з election) активний вузол
- Consistent hashing ring: user@domain → backend node
- Sticky sessions: всі з'єднання одного юзера йдуть на один backend
- Health checks до backend нод (gRPC ping)
- Failover: при падінні backend → reassign users → notify proxies
- gRPC API для proxy

### Backend
- Повноцінний IMAP/POP3/JMAP сервер
- Sieve, Quota, ACL, FTS
- Доступ до MailboxBackend + IndexBackend
- Не виставляється в інтернет — тільки всередині кластера
- Реєструється в Director при старті

---

### Deployment modes

| Mode | Компоненти | Use case |
|:---|:---|:---|
| Single node | proxy + director + backend в одному процесі | dev / small |
| Multi-node | окремі proxy / director / backend | production |
| Cloud | proxy + director + backend(obox+Cassandra) | large scale |

---

### Storage Layer

```
MailboxBackend (формат зберігання листів)
    |- Maildir    — local FS, один файл = один лист
    |- dbox       — local FS, sdbox формат
    |- mdbox      — local FS, кілька листів в файлі
    \- obox       — S3-compatible object storage

IndexBackend (де зберігається IMAP індекс)
    |- FileIndex  — бінарні файли поруч з mailbox (single node)
    |- SQLite     — dev / малі деплої
    \- Cassandra  — multi-node / large scale
```

Будь-яка комбінація: Maildir+FileIndex (single node),
mdbox+Cassandra (multi-node), obox+Cassandra (cloud).

---

## Directory Layout

```
yarilo/
|- cmd/
|  \- yarilo/             # єдиний бінарник, режим через конфіг
|- internal/
|  |- proxy/              # Proxy mode: TLS, auth, routing, connection proxy
|  |- director/           # Director mode: hash ring, sticky sessions, health
|  |- backend/            # Backend mode: wiring всіх компонентів
|  |- cluster/
|  |  |- grpc/            # gRPC протокол proxy<->director<->backend
|  |  \- ring/            # Consistent hashing ring
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
|  \- telemetry/          # Health + Prometheus (кожен інстанс)
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
    Delete(user, folder string, uid uint32) error
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
- Proxy + Director + Backend інтерфейси закладені з першого рядка коду
- Single-node режим: всі три в одному процесі
- IMAP4rev1 + основні extensions
- Maildir mailbox backend (local FS)
- FileIndex index backend
- Multi-tenant (user@domain)
- Auth: SQLite passdb
- TLS + SNI (per-domain certificates)
- IMAP IDLE (RFC 2177)
- CONDSTORE / QRESYNC (RFC 7162)
- `/healthz`, `/readyz`, `/metrics` на кожному інстансі

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
- [ ] `internal/cluster/ring` — consistent hashing
- [ ] `internal/cluster/grpc` — gRPC protobuf для міжкомпонентної комунікації
- [ ] `internal/proxy` — single-node stub (пряме з'єднання до backend)
- [ ] `internal/director` — single-node stub (локальний routing)
- [ ] `internal/storage/mailbox/maildir`
- [ ] `internal/storage/index/file`
- [ ] `internal/imap` — IMAP сервер (go-imap/v2)
- [ ] `internal/auth/sql` — SQLite passdb
- [ ] `internal/telemetry` — `/healthz`, `/readyz`, `/metrics`
- [ ] Config: `yarilo.yaml` з `mode: single`
- [ ] `cmd/yarilo` — єдиний бінарник
- [ ] Docker: `golang:1.26-alpine` → `alpine:3.23`
- [ ] GitHub Actions CI: build + test (linux/amd64)
- [ ] README: config reference, deploy guide

---

## Phase 2 — dbox + mdbox backends (target: +1-2 months)

### Goals
- dbox (sdbox) mailbox backend — Dovecot-compatible single-message format
- mdbox mailbox backend — multi-message files, highest density
- Flags stored in index only (like Dovecot dbox)
- Migration tool: Maildir → dbox

### Deliverables
- [ ] `internal/storage/mailbox/dbox`
- [ ] `internal/storage/mailbox/mdbox`
- [ ] `cmd/yarilo-migrate` — Maildir → dbox/mdbox converter
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
- [ ] `cmd/smtp` binary
- [ ] README update

---

## Phase 4 — Auth Backends (target: +1 month)

### Goals
- SQL passdb/userdb: SQLite, MySQL, PostgreSQL (один драйвер, різні DSN)
- OAuth2 / OIDC (XOAUTH2 SASL механізм)
- Passdb chaining

### Deliverables
- [ ] `internal/auth/sql` — єдиний SQL backend, підтримує SQLite / MySQL / PostgreSQL
- [ ] `internal/auth/oauth2` — OIDC discovery, token introspection, XOAUTH2
- [ ] passdb chain config
- [ ] README auth section

### Backlog (не в scope v1.0)
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
- [ ] `cmd/pop3` or merged binary
- [ ] README update

---

## Phase 10 — JMAP (target: +2-3 months)

### Goals
- JMAP Core (RFC 8620) — HTTP/HTTPS API
- JMAP Mail (RFC 8621) — mailbox, email, thread operations
- JMAP over WebSocket (RFC 8887) — push notifications
- Shared auth + mailbox + index backends з IMAP
- Порт 443 (HTTPS) + WebSocket upgrade

### Deliverables
- [ ] `internal/jmap` — JMAP Core + Mail handler (go-jmap)
- [ ] `internal/jmap/push` — WebSocket push (RFC 8887)
- [ ] `cmd/jmap` або merged binary
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
    backend: file           # file | sqlite | cassandra
    path: /var/lib/yarilo/index  # for file | sqlite
    cassandra:              # only when backend: cassandra
      hosts:
        - "cassandra-1:9042"
        - "cassandra-2:9042"
      keyspace: yarilo
      username: ""
      password: ""

auth:
  backends:
    - type: sql
      dsn: "/var/lib/yarilo/users.db"              # SQLite
      # dsn: "mysql://user:pass@localhost/yarilo"   # MySQL
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
  addr: ":995"
  tls: true

managesieve:
  addr: ":4190"

telemetry:
  addr: "0.0.0.0:9090"      # кожен інстанс (proxy/director/backend)
  health_path: /healthz      # GET -> 200 OK / 503
  ready_path:  /readyz       # GET -> 200 OK коли готовий до трафіку
  metrics_path: /metrics     # Prometheus exposition format

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
- [ ] FileIndex (binary, Dovecot-compatible format)
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

### Features
- [ ] Multi-tenant (user@domain)
- [ ] Per-user + per-domain quotas
- [ ] ACL / shared mailboxes
- [ ] Shared namespace
- [ ] FTS (SQLite FTS5)
- [ ] FTS (Elasticsearch)
- [ ] Admin REST API
- [ ] Prometheus metrics

---

## Release Milestones

| Milestone | Content | Target |
|:---|:---|:---|
| v0.1.0 | Phase 1 — IMAP + Maildir + FileIndex | Month 3 |
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
