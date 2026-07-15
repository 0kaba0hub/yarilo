# yarilo-auth audit and modernisation plan

This document captures the gap between **yarilo-auth** (current single-binary
authentication service) and **Dovecot 2.4's authentication architecture**, plus a
phased implementation plan that closes the most operationally-impactful gaps
first.

The audit was driven by a concrete blocker: backend-api needs auth-aware user
lookups (`/api/backend/user/info` enrichment, future folder write ops) and
yarilo-auth has no userdb-lookup capability on the wire. Rather than building a
narrow shim, this review steps back and looks at what else is missing so the
work plan does not leave us with a half-finished surface that has to be rebuilt
on the next operational requirement.

References:

- yarilo-auth: `internal/auth/protocol/protocol.go`, `internal/auth/sql/`
- Dovecot 2.4: `/Users/ihorru/Documents/GIT/igorru_dns/dovecot-2.4/src/auth/` and
  `src/lib-auth-client/auth-client-interface.h`

---

## Side-by-side comparison

### Wire protocols

|                                 | Dovecot 2.4                                                                | yarilo-auth                                          |
| :------------------------------ | :------------------------------------------------------------------------- | :--------------------------------------------------- |
| Client protocol version         | 1.4 (`auth-client-interface.h:8-9`)                                        | 1.0 (`protocol.go:22-23`)                            |
| Server→client handshake         | `VERSION`, `MECH×N`, `SPID`, `CUID`, `COOKIE`, `DONE`                      | same, but `MECH` lists only `PLAIN` + `LOGIN`        |
| Client→server handshake         | `VERSION` (mandatory) + `CPID` (mandatory)                                 | `CPID` parsed but ignored                            |
| Client commands                 | `AUTH` / `CONT` / `CANCEL`                                                 | `AUTH` / `CONT` (no-op) / `CANCEL` (no-op)           |
| Server responses                | `OK` / `FAIL` / `CONT`                                                     | `OK` / `FAIL` (no multi-round SASL — no `CONT` out)  |
| Pipelined requests via `<id>`   | yes — concurrent in-flight                                                 | `<id>` parsed but processed serially                 |
| Master protocol (admin lookups) | yes — `USER` / `PASS` / `LIST` / `REQUEST` / `CACHE-FLUSH` on a 2nd socket | **absent**                                           |

### Data model

| Field                            | Dovecot                                                  | yarilo                                               |
| :------------------------------- | :------------------------------------------------------- | :--------------------------------------------------- |
| `username`                       | mandatory                                                | mandatory                                            |
| `home`                           | in `auth-fields` bag                                     | dedicated `Home`                                     |
| `mail`                           | in `auth-fields` bag                                     | dedicated `MailLoc`                                  |
| `uid` / `gid`                    | in `auth-fields` (`auth-request.c:2220, 2227`)           | **absent**                                           |
| `quota_rule`                     | in `auth-fields`                                         | **absent**                                           |
| `allow_nets`                     | in `auth-fields` (`auth-request.c:2005`)                 | **absent**                                           |
| `nodelay` / `nologin`            | in `auth-fields` (`auth-request.c:2014`)                 | **absent**                                           |
| `system_groups_user`             | in `auth-fields` (`auth-request.c:2253`)                 | **absent**                                           |
| `master_user` / `original_user`  | dedicated fields + `master=` echo in OK                  | **absent**                                           |
| `userdb_*=` / `auth_*=` prefixes | gating which fields cross the wire (hidden internal)     | **no prefix semantics**                              |
| `proxy` / `host` / `port`        | in `auth-fields`                                         | dedicated fields (modelled but not wire-serialised)  |

### Passdb / userdb separation

Dovecot:

- `passdb { ... }` — verifies credentials; returns
  `OK / PASS_EXPIRED / USER_UNKNOWN / USER_DISABLED / PASSWORD_MISMATCH /
  NEXT / INTERNAL_FAILURE` (`passdb.h:13-24`).
- `userdb { ... }` — looks up uid/gid/home/groups **without a password**
  (`userdb.h:63-71`).
- Independent fallthrough chains: a passdb chain and a userdb chain, each
  with `NEXT` semantics.
- `userdb-prefetch` short-circuits the userdb lookup when the passdb already
  returned the userdb fields.

yarilo:

- One `Passdb` interface that does both (`protocol.go:47-52`).
- One fallthrough `Chain` (`protocol.go:54-69`).
- **No userdb-only lookup on the wire** — backend-api cannot answer "who is
  `alice`?" without a password.

### SASL mechanisms

Dovecot ships one `auth-sasl-mech-*.c` per mechanism:

- Plain: `PLAIN`, `LOGIN`
- Hash: `CRAM-MD5`, `DIGEST-MD5`, `APOP`
- SCRAM: `SCRAM-SHA-1`, `SCRAM-SHA-256`, and `-PLUS` channel-binding variants
- Kerberos: `GSSAPI`, `GSS-SPNEGO`
- External: `EXTERNAL` (TLS client cert), `OAUTH2` / `OAUTHBEARER`, `OTP`
- Dovecot-specific: `DOVECOT-TOKEN`, `ANONYMOUS`

yarilo: **`PLAIN` and `LOGIN` only** (`protocol.go:135-136`, `parsePlain`).
`CONT` is not implemented — no multi-round SASL.

### Operational features

| Feature                              | Dovecot                                              | yarilo |
| :----------------------------------- | :--------------------------------------------------- | :----- |
| Master users (`master=` impersonate) | yes (`auth-request.c:1073`, response field echo)     | absent |
| In-memory cache (LRU + TTL + neg-TTL)| `auth-cache.c`                                       | absent |
| Penalty / rate limit (per-IP backoff)| `auth-penalty.c` (IPv6 /48-mask, Anvil-backed)       | absent |
| Policy HTTP integration              | `auth-policy.c` (async POST to scoring URL)          | absent |
| Master protocol (USER / PASS / LIST) | `auth-master-connection.c`                           | absent |
| Blocking worker pool                 | child processes for PAM/LDAP/SQL                     | absent |
| Credential scheme negotiation        | `lookup_credentials` picks scheme SASL mech asks for | always plaintext compare |
| passdb backends                      | passwd / passwd-file / shadow / pam / bsdauth / sql / ldap / imap / oauth2 / static / lua | sql only |
| userdb backends                      | passwd / passwd-file / sql / ldap / static / lua / prefetch | none (model absent) |
| Snapshot/rollback on chain fallthrough | `auth_fields_snapshot/rollback`                    | absent |

### Verified wire bits

Master-protocol commands (verified at `auth-master-connection.c:293-369`):

```
NOTFOUND <id>
USER <id> <user>\t<fields>
PASS <id> user=<user>\t<fields>
```

Client-protocol version constants (`auth-client-interface.h:8-9`):

```c
#define AUTH_CLIENT_PROTOCOL_MAJOR_VERSION 1
#define AUTH_CLIENT_PROTOCOL_MINOR_VERSION 4
```

`AUTH_CLIENT_PROTOCOL_MINOR_VERSION` jumps gate features: `CANCEL` (v4),
TLS channel binding for SCRAM-PLUS (v3).

---

## Bottom line

yarilo-auth is **roughly 5 % of Dovecot's auth broker by feature surface**.
It covers one narrow channel — "verify a PLAIN/LOGIN password, return home /
mail" — and nothing else. Everything operational (userdb lookup, master users,
SASL diversity, caching, penalty, master protocol for admin tools, prefetch,
worker pool) is missing.

For the **immediate blocker** (backend-api auth-aware lookups + folder write
ops), the missing piece is the master-protocol + a `Userdb` interface. That
work is Phase AUTH-1 below; everything after is incremental and unblocks no
current consumer.

---

# Implementation plan

Phases are listed in priority order. Each phase is self-contained and does
not block subsequent phases. Concrete PR breakdown per phase happens in a
separate planning step right before the phase starts — these summaries are
scoping only, not commitments.

## Phase AUTH-1 — Userdb foundation **(unblocks the current stream)**

Motivation: closes the backend-api blocker. Backend admin API can finally
call userdb to enrich `/api/backend/user/info` and authorise folder write ops
without piggy-backing on full AUTH.

Work:

1. `protocol.Userdb` interface — `Lookup(username string) (*UserInfo, error)`,
   password-less.
2. `protocol.UserInfo` struct — explicit fields (uid, gid, home, mail,
   allow_nets, quota_rule, groups) plus an `Extra map[string]string` bag for
   forward-compat. Marshalling helpers shared with `AuthResponse`.
3. Master-protocol wire — separate listener with its own mTLS block. Commands:
   `USER <id> <user>` → `USER <id> <user>\t<fields>` or `NOTFOUND <id>` /
   `FAIL <id> reason=...`. `PASS <id> <user>` for credential lookup without
   the actual auth dance. `LIST` for iteration. ID-tracked, pipelinable.
4. `pkg/authclient` — Go client that speaks both client and master protocols.
   `Dial(addr, mtls)` returns one connection that supports both `Auth(...)`
   and `Userdb(...)`. Connection pool inside.
5. SQL userdb driver — parallel to the existing `auth/sql.Passdb`, reads the
   same DB but executes a userdb query (configurable, default same JOIN as the
   passdb query minus the password column).
6. `yarilo-auth` listens on **two sockets**: existing client socket + new
   master socket. Each has its own mTLS config.
7. Config: `auth_service.master_listen:` plus mtls block under
   `auth_service.master_mtls:`.
8. Wire `authclient` into `yarilo-backend-api/main.go` so the existing
   blocker work can land immediately after AUTH-1 merges.

Out of Phase AUTH-1: master users, cache, penalty, policy, additional SASL
mechs, additional passdb/userdb drivers.

## Phase AUTH-2 — Extra fields + prefetch

1. Replace the fixed-field `AuthResponse` with an `auth_fields`-style bag —
   key/value pairs with a flags mask (`hidden`, `changed`, `userdb`).
2. Wire serialisation — `userdb_*=` / `auth_*=` prefix gating on response so
   internal fields never cross the wire.
3. Passdb → userdb prefetch — when passdb sets userdb-typed fields, userdb
   lookup is skipped.
4. snapshot / rollback for fallthrough between chained passdbs.
5. Reserved-field handling (uid, gid, mail, home, quota_rule, allow_nets,
   nodelay, nologin, system_groups_user) with parsing tests.

## Phase AUTH-3 — Master users

1. Wire: `master=<masteruser>` in the `AUTH` request.
2. Separate `masterdb` chain (subset of passdb chain marked master-only).
3. Two-stage flow: passdb verifies the master password → masterdb authorises
   the target → `request.user` switches to the target.
4. Echo `master=` in the `OK` response for audit visibility.

## Phase AUTH-4 — Cache + penalty

1. Cache — LRU + positive-TTL + negative-TTL (`auth-cache.c` shape). Cache
   key with var-substitution (`%u`, `%n`, `%d`, `%r`). `CACHE-FLUSH` command
   on the master socket.
2. Penalty — integrate with `internal/anvil` (we already have `COUNTER-INC`
   primitives). Per-IP counter with exponential backoff, IPv6 masked to /48.

## Phase AUTH-5 — Additional SASL mechs

Pick up per operator demand:

1. `EXTERNAL` (TLS client cert) — internal automation, CI.
2. `CRAM-MD5` — pre-TLS legacy clients (low priority).
3. `SCRAM-SHA-256` — modern best-practice; needs passdb credential-scheme
   storage.
4. `SCRAM-SHA-256-PLUS` — TLS channel binding.
5. `OAUTH2` / `OAUTHBEARER` — IdP integration.
6. `GSSAPI` / `GSS-SPNEGO` — Kerberos (enterprise).

Each mech is its own mini-phase with a `mechanisms: [...]` config knob.

## Phase AUTH-6 — Policy + worker pool

Low priority, does not block anything. Pull when concrete need surfaces.

1. Policy HTTP integration — async POST to configurable URL with JSON
   payload.
2. Blocking worker pool for slow passdbs (PAM, LDAP).

## Phase AUTH-7 — Additional passdb / userdb drivers

By operator demand:

- `passwd-file` — self-hosted simple deployments — **done** (passdb + userdb)
- `static` — tests + single-mailbox setups — **done** (passdb + userdb)
- `ldap` — enterprise — backlog (#558)
- `pam` — UNIX shell users — backlog (#558)
- `lua` — escape hatch — backlog (#558)
- `imap` — proxy fallback — backlog (#558)

---

## Recommendation

Land **Phase AUTH-1 first**. It is the smallest phase that unblocks the
current stream (backend-api auth-aware lookups + folder write ops). Everything
in AUTH-2 through AUTH-7 is operational improvement that is not blocking any
known consumer today; pull them when the corresponding need surfaces.
