# yarilo — deferred work

Items that are known-needed but were intentionally not shipped in
the current phase. Each entry is one paragraph: what it is, why it
was deferred, and what would unblock it.

Items get removed only when the corresponding work merges to `main`.

---

## Phase AUTH-5 — additional SASL mechanisms

Currently shipped: PLAIN, OAUTHBEARER, XOAUTH2, SCRAM-SHA-256 (+PLUS),
SCRAM-SHA-1 (+PLUS).

Still missing, by demand order:
`EXTERNAL` → `CRAM-MD5` → `GSSAPI`.

Each mechanism ships as its own PR with a `mechanisms: [...]` config
knob. `go-sasl` fork (`0kaba0hub/go-sasl`) already has server-side
SCRAM/XOAUTH2/CRAM-MD5/DIGEST-MD5 on `yarilo-patches` — pick up from
there for CRAM-MD5.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-5.

---

## Phase AUTH-7 — additional passdb / userdb drivers

Currently shipped: SQL (sqlite/mysql/postgres) + OAuth2.

Still missing, by operator demand order:
`passwd-file` → `ldap` → `pam` → `lua` → `static` → `imap`.

Order driven by ticket pressure, not pre-decided.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-7.

---

## Phase OBOX-1 — object-storage mailbox backend

Sketch lives in
[memory:project_yarilo_phase_obox_backlog.md]. S3-/blob-backed
mailbox backend behind the existing `pkg/mailbox` interface, plus
the design references already captured (Stalwart, Apache James).
Deferred — no priority; standalone deployment must work first.

---

## ManageSieve / Sieve (Phase SIEVE-1)

Script storage, parser, execution engine on incoming mail, RFC
5804 wire protocol on port 4190. Big phase — deferred until ACL
and quota are stable.

---

## Replication (dsync) — Phase REPL-1

Mailbox sync between replicas — sketch deferred until backend
architecture is fully proven in production.

---

## FTS — full-text search (Phase FTS-1)

Indexer + SEARCH BODY/TEXT optimisation. Big phase, no current
ETA.

