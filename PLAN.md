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
              │  └──────────┘  └──────────┘  │   API    │  │
              │                              └──────────┘  │
              └──────────────────┬──────────────────────────┘
                                 │
              ┌──────────────────┼────────────────────────┐
              │            Storage Layer                   │
              │                                           │
              │   ┌─────────────────┐  ┌──────────────┐  │
              │   │   PostgreSQL    │  │      S3      │  │
              │   │   (metadata)   │  │   (bodies)   │  │
              │   │  mailboxes     │  │              │  │
              │   │  messages idx  │  │  raw message │  │
              │   │  flags/UIDs    │  │  files       │  │
              │   │  quotas/ACLs   │  │              │  │
              │   └─────────────────┘  └──────────────┘  │
              └───────────────────────────────────────────┘
```

---

## Directory Layout

```
yarilo/
|- cmd/
|  |- imap/          # IMAP server entrypoint
|  |- smtp/          # SMTP inbound MTA entrypoint
|  |- lmtp/          # LMTP local delivery entrypoint
|  \- admin/         # Admin API entrypoint
|- internal/
|  |- imap/          # IMAP protocol handlers (go-imap/v2 backend)
|  |- smtp/          # SMTP inbound + outbound
|  |- lmtp/          # LMTP delivery agent
|  |- pop3/          # POP3 protocol handler
|  |- storage/
|  |  |- postgres/   # PostgreSQL metadata backend
|  |  |- s3/         # S3 message body backend
|  |  \- fs/         # Local filesystem fallback
|  |- auth/
|  |  |- sql/        # SQL passdb/userdb
|  |  |- ldap/       # LDAP passdb/userdb
|  |  |- pam/        # PAM passdb
|  |  \- oauth2/     # OAuth2/OIDC passdb
|  |- sieve/         # Sieve script engine (RFC 5228)
|  |- managesieve/   # ManageSieve protocol (RFC 5804)
|  |- quota/         # Quota tracking and enforcement
|  |- acl/           # ACL and shared mailboxes
|  |- fts/           # Full-text search indexer
|  |- dkim/          # DKIM sign + verify
|  |- spf/           # SPF verification
|  |- dmarc/         # DMARC policy evaluation
|  \- admin/         # Admin HTTP API
|- pkg/
|  |- mailbox/       # Core mailbox types shared across packages
|  \- config/        # Configuration parsing
|- migrations/       # PostgreSQL migrations (goose)
|- config/
|  \- yarilo.yaml    # Example config
|- doc/
|  |- presentation.md
|  \- presentation.pdf
\- docker/
   \- Dockerfile
```

---

## Protocol Library Strategy

| Layer | Library | Why |
|:---|:---|:---|
| IMAP wire format | `github.com/emersion/go-imap/v2` | RFC 3501/9051 + extensions, MIT |
| SMTP wire format | `github.com/emersion/go-smtp` | RFC 5321, same author |
| Sieve engine | `github.com/emersion/go-sieve` | RFC 5228, same ecosystem |
| S3 client | `github.com/minio/minio-go/v7` | S3-compatible, production |
| PostgreSQL | `github.com/jackc/pgx/v5` | native driver, no ORM |
| Migrations | `github.com/pressly/goose/v3` | embedded migrations |
| TLS / SNI | stdlib `crypto/tls` | per-domain cert loading |
| Logging | `log/slog` JSON handler | standard, structured |
| Config | `github.com/knadh/koanf/v2` | YAML + env override |

---

## PostgreSQL Schema (core tables)

```sql
-- domains
domains (id, name, created_at)

-- users / mailboxes
users (id, domain_id, username, password_hash, quota_bytes, active, created_at)

-- folders (IMAP mailboxes)
folders (id, user_id, name, uidvalidity, uidnext, subscribed)

-- messages metadata
messages (
  id, folder_id, uid, size_bytes,
  internal_date, flags, body_key,   -- body_key = S3 object key
  modseq                            -- for CONDSTORE / QRESYNC
)

-- quotas
quota_usage (user_id, used_bytes, message_count)

-- ACL
acl_entries (folder_id, identifier, rights)

-- Sieve scripts
sieve_scripts (id, user_id, name, active, content, created_at)

-- FTS index (if not using external)
fts_index (message_id, tokens tsvector)

-- sessions (for admin)
sessions (id, user_id, token_hash, expires_at)
```

---

## Phase 1 — Core IMAP + Storage (target: 2-3 months)

### Goals
- Production-grade IMAP4rev1 server
- PostgreSQL metadata + S3 message bodies
- Multi-tenant (user@domain)
- Basic SQL auth
- TLS + SNI (per-domain certificates)
- IMAP IDLE (RFC 2177)
- CONDSTORE / QRESYNC (RFC 7162)

### IMAP Extensions to implement
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
- [ ] PostgreSQL schema + goose migrations
- [ ] S3 storage backend (minio-go)
- [ ] IMAP backend implementing `go-imap/v2` interfaces
- [ ] SQL auth passdb/userdb
- [ ] TLS with SNI (per-domain certs)
- [ ] Config: `yarilo.yaml` with domains, listeners, storage, auth
- [ ] `cmd/imap` binary
- [ ] Docker: `golang:1.26-alpine` builder → `alpine:3.23` runtime
- [ ] GitHub Actions CI: build + test (linux/amd64)
- [ ] README with env vars, config reference, deploy guide

---

## Phase 2 — SMTP Inbound + Delivery (target: +1-2 months)

### Goals
- Receive inbound email (MX)
- LMTP local delivery to IMAP storage
- DKIM verification
- SPF verification
- DMARC policy evaluation
- Anti-spam integration (rspamd via milter protocol)
- DKIM signing for outbound

### Deliverables
- [ ] `internal/smtp` — inbound SMTP server (go-smtp)
- [ ] `internal/lmtp` — LMTP delivery to IMAP storage
- [ ] `internal/dkim` — sign + verify
- [ ] `internal/spf` — SPF lookup + verify
- [ ] `internal/dmarc` — DMARC policy
- [ ] rspamd milter client
- [ ] SMTP submission server (port 587, AUTH required)
- [ ] `cmd/smtp` binary
- [ ] README update

---

## Phase 3 — Auth Backends (target: +1 month)

### Goals
- LDAP passdb + userdb (BindDN + lookup)
- OAuth2 / OIDC (XOAUTH2 SASL mechanism)
- PAM passdb (via cgo or helper binary)
- Multiple passdb chains (try SQL first, fallback LDAP)

### Deliverables
- [ ] `internal/auth/ldap`
- [ ] `internal/auth/oauth2`
- [ ] `internal/auth/pam`
- [ ] passdb chain support in config
- [ ] README auth section

---

## Phase 4 — Sieve + ManageSieve (target: +1-2 months)

### Goals
- Server-side Sieve script execution on delivery (RFC 5228)
- ManageSieve protocol for remote script management (RFC 5804)
- Extensions: `fileinto`, `reject`, `vacation`, `imap4flags`, `copy`, `envelope`

### Deliverables
- [ ] `internal/sieve` — script execution engine (go-sieve)
- [ ] `internal/managesieve` — protocol server (port 4190)
- [ ] Sieve script storage in PostgreSQL
- [ ] Integration with LMTP delivery pipeline
- [ ] `cmd/imap` updated to include ManageSieve listener
- [ ] README update

---

## Phase 5 — Quota + ACL + Shared Mailboxes (target: +1-2 months)

### Goals
- Per-user and per-domain quotas (storage bytes + message count)
- ACL plugin: share mailbox folders between users (RFC 4314)
- Shared namespace: `/shared/` prefix in IMAP
- Quota enforcement on APPEND and COPY

### IMAP Extensions
- `QUOTA` — RFC 9208
- `ACL` — RFC 4314
- `MYRIGHTS` — RFC 4314

### Deliverables
- [ ] `internal/quota` — tracking + enforcement
- [ ] `internal/acl` — ACL storage + IMAP commands
- [ ] Shared namespace in IMAP NAMESPACE response
- [ ] README update

---

## Phase 6 — POP3 (target: +1 month)

### Goals
- Full POP3 server (RFC 1939)
- UIDL support
- Reuse same auth and storage backends as IMAP

### Deliverables
- [ ] `internal/pop3` — protocol handler
- [ ] `cmd/pop3` or merged into combined binary
- [ ] README update

---

## Phase 7 — Full-Text Search (target: +1-2 months)

### Goals
- PostgreSQL `tsvector` based FTS (no external dependency)
- Optional: Elasticsearch/Opensearch backend for large deployments
- IMAP `SEARCH` enhancement with body search

### Deliverables
- [ ] `internal/fts/postgres` — tsvector indexer
- [ ] `internal/fts/elastic` — Elasticsearch backend
- [ ] FTS triggered on message delivery
- [ ] IMAP SEARCH extended to use FTS index
- [ ] README update

---

## Phase 8 — Admin API + Provisioning (target: +1-2 months)

### Goals
- REST API for domain/user/mailbox management
- Create/delete/suspend users
- Quota management
- Per-domain settings
- Metrics endpoint (Prometheus)

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
- [ ] `internal/admin` — HTTP API (stdlib net/http)
- [ ] `cmd/admin` binary (or merged)
- [ ] API key auth for admin endpoints
- [ ] Prometheus metrics: active connections, messages/sec, storage used
- [ ] README API section

---

## Configuration Reference (yarilo.yaml)

```yaml
log_level: info   # debug | info | warn | error

storage:
  postgres:
    dsn: "postgres://user:pass@localhost/yarilo?sslmode=disable"
  s3:
    endpoint: "s3.amazonaws.com"
    bucket: "yarilo-mail"
    region: "eu-central-1"
    access_key: ""
    secret_key: ""

auth:
  backends:
    - type: sql
      dsn: "postgres://..."
    - type: ldap
      url: "ldap://localhost:389"
      bind_dn: "cn=admin,dc=example,dc=com"
      bind_password: ""
      user_filter: "(mail=%s)"

imap:
  listeners:
    - addr: ":993"
      tls: true
      cert: "/etc/yarilo/certs/fullchain.pem"
      key:  "/etc/yarilo/certs/privkey.pem"
    - addr: ":143"
      starttls: true
  idle_timeout: 30m

smtp:
  inbound:
    addr: ":25"
    hostname: "mail.example.com"
    tls: true
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
| `LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `POSTGRES_DSN` | --- | Overrides storage.postgres.dsn |
| `S3_ENDPOINT` | --- | Overrides storage.s3.endpoint |
| `S3_BUCKET` | --- | Overrides storage.s3.bucket |
| `S3_ACCESS_KEY` | --- | S3 access key |
| `S3_SECRET_KEY` | --- | S3 secret key |
| `ADMIN_API_KEY` | --- | Admin API key |

---

## Feature Parity Checklist

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

### Storage
- [ ] PostgreSQL metadata backend
- [ ] S3-compatible message body backend
- [ ] Local filesystem fallback

### Auth
- [ ] SQL passdb/userdb
- [ ] LDAP passdb/userdb
- [ ] PAM passdb
- [ ] OAuth2/OIDC (XOAUTH2)
- [ ] Passdb chaining (multiple backends)

### Filtering
- [ ] Sieve script engine (RFC 5228)
- [ ] ManageSieve protocol (RFC 5804)
- [ ] fileinto, reject, vacation, imap4flags extensions

### Security
- [ ] TLS 1.2 + 1.3
- [ ] SNI (per-domain certificates)
- [ ] DKIM sign + verify
- [ ] SPF verification
- [ ] DMARC policy evaluation
- [ ] Anti-spam (rspamd milter)

### Features
- [ ] Multi-tenant (user@domain)
- [ ] Per-user quotas (bytes + count)
- [ ] Per-domain quotas
- [ ] ACL / shared mailboxes
- [ ] Shared namespace
- [ ] Full-text search (PostgreSQL tsvector)
- [ ] Full-text search (Elasticsearch)
- [ ] Admin REST API
- [ ] Prometheus metrics

---

## Release Milestones

| Milestone | Content | Target |
|:---|:---|:---|
| v0.1.0 | Phase 1 — IMAP + Storage | Month 3 |
| v0.2.0 | Phase 2 — SMTP + Delivery | Month 5 |
| v0.3.0 | Phase 3 — Auth backends | Month 6 |
| v0.4.0 | Phase 4 — Sieve + ManageSieve | Month 8 |
| v0.5.0 | Phase 5 — Quota + ACL | Month 10 |
| v0.6.0 | Phase 6 — POP3 | Month 11 |
| v0.7.0 | Phase 7 — FTS | Month 13 |
| v1.0.0 | Phase 8 — Admin API + production hardening | Month 15 |
