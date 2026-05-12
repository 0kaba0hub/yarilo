# Yarilo — Implementation Plan

Yarilo is a production-grade IMAP/SMTP mail server written in Go,
targeting full feature parity with industry-standard open-source IMAP servers.

---

## Architecture

```
                     ┌─────────────────────────────────────┐
                     │              Clients                 │
                     │  Thunderbird / Apple Mail / Outlook  │
                     └────────┬──────────────┬─────────────┘
                              │ IMAP/POP3    │ SMTP submit
                 ┌────────────▼──────────┐   │
                 │     IMAP Server       │   │
                 │  (go-imap/v2 backend) │   │
                 └────────────┬──────────┘   │
                              │              │
           ┌──────────────────┼──────────────▼──────────┐
           │              Yarilo Core                    │
           │                                             │
           │  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
           │  │   Auth   │  │  Quota   │  │   ACL    │  │
           │  └──────────┘  └──────────┘  └──────────┘  │
           │  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
           │  │  Sieve   │  │   FTS    │  │  Admin   │  │
           │  └──────────┘  └──────────┘  └──────────┘  │
           └──────────────────┬──────────────────────────┘
                              │
        ┌─────────────────────┴──────────────────────────┐
        │                 Storage Layer                   │
        │                                                 │
        │   MailboxBackend          IndexBackend          │
        │   ┌──────────────┐        ┌──────────────┐      │
        │   │   Maildir    │        │  FileIndex   │      │
        │   │   dbox       │   +    │  SQLite      │      │
        │   │   mdbox      │        │  Cassandra   │      │
        │   │   obox (S3)  │        └──────────────┘      │
        │   └──────────────┘                              │
        └─────────────────────────────────────────────────┘
```

**MailboxBackend** — як і де зберігаються тіла листів (формат)
**IndexBackend** — де зберігається індекс (UIDs, flags, modseq, folders)

Будь-яка комбінація: наприклад Maildir + FileIndex (single node),
або mdbox + Cassandra (multi-node), або obox + Cassandra (cloud).

---

## Directory Layout

```
yarilo/
|- cmd/
|  |- imap/               # IMAP server entrypoint
|  |- smtp/               # SMTP inbound MTA entrypoint
|  |- lmtp/               # LMTP local delivery entrypoint
|  \- admin/              # Admin API entrypoint
|- internal/
|  |- imap/               # IMAP protocol handlers (go-imap/v2 backend)
|  |- smtp/               # SMTP inbound + outbound
|  |- lmtp/               # LMTP delivery agent
|  |- pop3/               # POP3 protocol handler
|  |- storage/
|  |  |- mailbox/
|  |  |  |- maildir/      # Maildir format (one file = one message)
|  |  |  |- dbox/         # sdbox format (Dovecot-compatible)
|  |  |  |- mdbox/        # mdbox format (multi-message files)
|  |  |  \- obox/         # Object storage (S3-compatible)
|  |  \- index/
|  |     |- file/         # File-based index (like Dovecot lib-index)
|  |     |- sqlite/       # SQLite index (dev / small deployments)
|  |     \- cassandra/    # Cassandra index (multi-node / large scale)
|  |- auth/
|  |  |- sql/             # SQL passdb/userdb (SQLite or external DB)
|  |  |- ldap/            # LDAP passdb/userdb
|  |  |- pam/             # PAM passdb
|  |  \- oauth2/          # OAuth2/OIDC passdb
|  |- sieve/              # Sieve script engine (RFC 5228)
|  |- managesieve/        # ManageSieve protocol (RFC 5804)
|  |- quota/              # Quota tracking and enforcement
|  |- acl/                # ACL and shared mailboxes
|  |- fts/                # Full-text search
|  |  |- sqlite/          # SQLite FTS5 (single node)
|  |  \- elastic/         # Elasticsearch (large scale)
|  |- dkim/               # DKIM sign + verify
|  |- spf/                # SPF verification
|  |- dmarc/              # DMARC policy evaluation
|  \- admin/              # Admin HTTP API
|- pkg/
|  |- mailbox/            # Core mailbox interfaces + types
|  \- config/             # Configuration parsing
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
| S3 client (obox) | `github.com/minio/minio-go/v7` | S3-compatible, production |
| SQLite | `modernc.org/sqlite` | pure Go, no cgo |
| Cassandra | `github.com/gocql/gocql` | mature driver |
| TLS / SNI | stdlib `crypto/tls` | per-domain cert loading |
| Logging | `log/slog` JSON handler | standard, structured |
| Config | `github.com/knadh/koanf/v2` | YAML + env override |

---

## Phase 1 — Core IMAP + Maildir + FileIndex (target: 2-3 months)

### Goals
- Production-grade IMAP4rev1 server
- Maildir mailbox backend (local FS)
- FileIndex index backend (binary files, like Dovecot lib-index)
- Multi-tenant (user@domain)
- Auth: flat file + SQLite passdb
- TLS + SNI (per-domain certificates)
- IMAP IDLE (RFC 2177)
- CONDSTORE / QRESYNC (RFC 7162)

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
- [ ] `MailboxBackend` + `IndexBackend` interfaces
- [ ] `internal/storage/mailbox/maildir` implementation
- [ ] `internal/storage/index/file` implementation
- [ ] IMAP server wiring `go-imap/v2`
- [ ] SQLite auth passdb/userdb
- [ ] TLS with SNI (per-domain certs)
- [ ] Config: `yarilo.yaml`
- [ ] `cmd/imap` binary
- [ ] Docker: `golang:1.26-alpine` builder → `alpine:3.23` runtime
- [ ] GitHub Actions CI: build + test (linux/amd64)
- [ ] README with config reference, deploy guide

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
- LDAP passdb + userdb
- OAuth2 / OIDC (XOAUTH2 SASL)
- PAM passdb
- Passdb chaining

### Deliverables
- [ ] `internal/auth/ldap`
- [ ] `internal/auth/oauth2`
- [ ] `internal/auth/pam`
- [ ] passdb chain config
- [ ] README auth section

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
- UIDL support
- Reuses same auth + mailbox + index backends

### Deliverables
- [ ] `internal/pop3`
- [ ] `cmd/pop3` or merged binary
- [ ] README update

---

## Phase 10 — Full-Text Search (target: +1-2 months)

### Goals
- SQLite FTS5 — zero deps, single node
- Elasticsearch backend — large deployments
- Triggered on delivery, IMAP SEARCH extended

### Deliverables
- [ ] `internal/fts/sqlite` — FTS5
- [ ] `internal/fts/elastic` — Elasticsearch
- [ ] README update

---

## Phase 11 — Admin API + Provisioning (target: +1-2 months)

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
      dsn: "/var/lib/yarilo/users.db"   # SQLite
    - type: ldap
      url: "ldap://localhost:389"
      bind_dn: "cn=admin,dc=example,dc=com"
      bind_password: ""
      user_filter: "(mail=%s)"

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
- [ ] IMAP4rev1 (RFC 3501)
- [ ] IMAP4rev2 (RFC 9051)
- [ ] POP3 (RFC 1939)
- [ ] LMTP (RFC 2033)
- [ ] SMTP inbound (RFC 5321)
- [ ] SMTP submission (RFC 6409)
- [ ] ManageSieve (RFC 5804)

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
- [ ] SQL / SQLite passdb/userdb
- [ ] LDAP passdb/userdb
- [ ] PAM passdb
- [ ] OAuth2/OIDC (XOAUTH2)
- [ ] Passdb chaining

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
| v0.9.0 | Phase 9 + 10 — POP3 + FTS | Month 18 |
| v1.0.0 | Phase 11 — Admin API + hardening | Month 20 |
