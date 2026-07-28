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

See [AUTH_REVIEW.md](AUTH_REVIEW.md) §Phase AUTH-5.

---

## Phase AUTH-7 — additional passdb / userdb drivers

Currently shipped: SQL (sqlite/mysql/postgres) + OAuth2.

Still missing, by operator demand order:
`passwd-file` → `ldap` → `pam` → `lua` → `static` → `imap`.

Order driven by ticket pressure, not pre-decided.

See [AUTH_REVIEW.md](AUTH_REVIEW.md) §Phase AUTH-7.

---

## Phase OBOX-1 — object-storage mailbox backend

Sketch lives in
[memory:project_yarilo_phase_obox_backlog.md]. S3-/blob-backed
mailbox backend behind the existing `pkg/mailbox` interface, plus
the design references already captured (Stalwart, Apache James).
Deferred — no priority; standalone deployment must work first.

---

## Replication (dsync) — Phase REPL-1

Mailbox sync between replicas — sketch deferred until backend
architecture is fully proven in production. This is also the base for a
cross-server **live migration** (dsync / imapc pulling mail over IMAP from
another server); the shipped `yarilo-migrate` only converts on-disk mailbox
formats offline, it does not sync from a remote server.

---

## FTS — remaining engines & scaling (Phase FTS-2+)

The **flatcurve (Xapian) engine is shipped**: `yarilo-fts` owns indexing and
lookups end-to-end, write-through-at-delivery + autoindex, per-mailbox
checkpoints, multi-language tokenization, sealed-shard rotation with background
compaction, and optional attachment decoders. See [FTS.md](FTS.md).

Still deferred:

- **Bleve v2 / scorch engine** — a pure-Go, CGo-free alternative to flatcurve for
  deployments that cannot link libxapian, registered behind the same `pkg/fts`
  engine interface (blank-import activation, mirroring `pkg/dict`). A separate
  follow-up stream — see [FTS.md](FTS.md) §9.2.
- **Solr / external-index engine** — HTTP-client engine for very large multi-node
  deployments; no current demand.
- **Index scaling** — single-writer-per-mailbox coordination and indexer-worker
  queueing under a shared FTS pool. Tracked in the FTS-2 / FTS-3 issues and #675.
