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

## yarilo-quota-status — per-user quota limits via userdb lookup

`yarilo-quota-status` (Postfix policy service, port 12340) is implemented and
deployed. Currently it enforces only `defaultQuotaRules` — cluster-wide limits
applied to every recipient. Per-user limits (individual `quota_rule` values
stored in userdb) are not yet wired in (`internal/quotastatus/server.go:27`).

Remaining work:

- Wire `pkg/authclient` master protocol into `yarilo-quota-status` to call a
  userdb `USER` lookup for the recipient address.
- Replace (or supplement) `defaultQuotaRules` with the per-user `quota_rule`
  field from `AuthResponse.Fields` (reserved field, already parsed by AUTH-2).
- Expose `quotaStatus.authAddr` / `quotaStatus.masterAddr` in `helm/values.yaml`
  and the `quotaStatus` config section.

Not blocked — AUTH-1 master protocol and userdb are already implemented; this
is purely wiring them into `yarilo-quota-status`.

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

