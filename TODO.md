# yarilo — deferred work

Items that are known-needed but were intentionally not shipped in
the current phase. Each entry is one paragraph: what it is, why it
was deferred, and what would unblock it.

Items get removed only when the corresponding work merges to `main`.

---

## yarilo-sasl-login — Dovecot SASL proxy for Postfix

Postfix supports delegating SMTP AUTH to a Dovecot SASL service
(`smtpd_sasl_type = dovecot`, `smtpd_sasl_path = private/auth`). This lets
Postfix authenticate submission clients via yarilo-auth without direct access
to the internal auth socket.

Implement `yarilo-sasl-login` — a dedicated binary that:

- implements the Dovecot SASL client protocol (Unix socket or TCP, equivalent
  to Dovecot's `service auth { socket listen { client { ... } } }`);
- receives AUTH requests from Postfix and proxies them to `yarilo-auth`;
- returns only `OK` / `FAIL` to Postfix — Postfix has no visibility into the
  yarilo-auth socket.

`auth_service.sasl_listen` config field already exists in
`pkg/config/config.go` (`SASLListen`) but is not wired to any binary.

Helm: new Deployment `yarilo-sasl-login` + `components.saslLogin` section in
`values.yaml`. Unix socket mode: mounted in the same PV as Postfix. TCP mode:
for k8s deployments where Postfix runs as a separate pod.

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

In Dovecot, Sieve is the separate Pigeonhole project — not part of the core
source tree. It hooks into LMTP delivery via a `deliver_mail_func_t` function
pointer registered during plugin init (`mail_deliver_hook_set()`). ManageSieve
(RFC 5804, port 4190) is a separate binary with its own login proxy, not part
of the Sieve execution engine.

For yarilo the architecture should mirror this split:

- **Sieve execution** — a delivery hook registered by a Sieve plugin, called
  from `yarilo-lmtp` after auth and before final mailbox delivery. Script
  storage: flat `.sieve` files under the user's home (same path conventions
  as Pigeonhole). No external library required for core actions (fileinto,
  redirect, reject, keep); vacation needs RFC 5230 + SMTP relay.
- **ManageSieve server** — separate binary `yarilo-managesieve` + login proxy
  `yarilo-managesieve-login`, port 4190. Speaks RFC 5804 for script upload,
  delete, activate, list.
- **Script storage abstraction** — needs investigation: Pigeonhole supports
  flat files and dict (SQL) backends; design the same interface for yarilo.

Architecture decision pending before implementation starts: whether the Sieve
execution engine is compiled-in (registered via blank import like dict drivers)
or a separate process. Dovecot uses dynamic `.so`; yarilo should prefer
compiled-in registration to avoid the Go plugin package complexity.

---

## Replication (dsync) — Phase REPL-1

Mailbox sync between replicas — sketch deferred until backend
architecture is fully proven in production.

---

## FTS — full-text search (Phase FTS-1)

IMAP `SEARCH BODY` and `SEARCH TEXT` are specified in RFC 3501 §6.4.4 and
RFC 9051 §6.4.4. FTS is an implementation optimisation of those criteria —
no separate RFC governs the indexer itself. `SEARCH=FUZZY` (RFC 6203) is a
related but distinct extension.

Architecture: plugin system identical to `pkg/dict` — each backend registers
itself via `init()` and is activated with a blank import:

```
pkg/fts/
  fts.go             — Indexer interface: Index(msg), Search(query), Delete(uid)
  registry.go        — Register / Open (mirrors pkg/dict/registry.go)

pkg/fts/backends/
  bleve/             — embedded Go-native indexer, zero external deps
  flatcurve/         — Xapian-based, Dovecot fts_flatcurve-compatible on-disk format
  solr/              — HTTP client for Apache Solr
  all/all.go         — blank-imports all backends

internal/imap/
  search_fts.go      — dispatch SEARCH BODY/TEXT to fts.Open() when configured
```

Config: `fts.backend: bleve` (or `xapian`, `solr`). Default: empty = no FTS,
SEARCH BODY/TEXT falls back to full scan (current behaviour preserved).

`bleve` is the recommended default for self-hosted deployments — pure Go,
no CGo, ships in the same binary. `flatcurve`/`xapian` requires CGo + libxapian.

