# yarilo — deferred work

Items that are known-needed but were intentionally not shipped in
the current phase. Each entry is one paragraph: what it is, why it
was deferred, and what would unblock it.

Items get removed only when the corresponding work merges to `main`.

---

## Phase AUTH-1 — userdb foundation (NEXT — unblocks the current stream)

Adds a `Userdb` interface, a master-protocol wire (`USER` / `PASS` /
`LIST`) on a separate mTLS socket, `pkg/authclient` (Go client that
speaks both client and master protocols), and a SQL userdb driver
parallel to the existing SQL passdb. Wires `authclient` into
`yarilo-backend-api/main.go` so the admin-API can finally do
password-less user lookups.

**Why first:** unblocks "Backend admin API — auth-aware user lookups"
and "Backend admin API — folder write ops" (both below) which are
otherwise stuck. Smallest auth-side step that closes the current
operational gap.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-1 for the
detailed item list. Out of scope: master users, cache, penalty,
policy, extra SASL mechs, extra passdb/userdb drivers — those are
their own phases (AUTH-2 .. AUTH-7) below.

---

## Phase AUTH-2 — extra fields + prefetch

Replaces the fixed-field `AuthResponse` with an `auth_fields`-style
bag (Dovecot's design): key/value with a flags mask (hidden,
changed, userdb), `userdb_*=` / `auth_*=` prefix gating on the wire,
passdb → userdb prefetch short-circuit, snapshot/rollback at
chain-fallthrough, reserved fields (uid, gid, quota_rule, allow_nets,
nodelay, nologin, system_groups_user) with parsing tests.

Not currently blocking any consumer; pull when the rigid
`AuthResponse` struct starts hurting (typically when AUTH-3 lands and
needs `master_user`/`original_user`, or when AUTH-7 adds drivers that
return new fields).

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-2.

---

## Phase AUTH-3 — master users

`master=<masteruser>` in `AUTH`, a separate masterdb chain, two-stage
flow (passdb verifies the master password → masterdb authorises the
target → `request.user` switches to the target), echo `master=` in
the `OK` response for audit visibility.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-3.

---

## Phase AUTH-4 — cache + penalty

In-memory LRU cache with positive-TTL + negative-TTL (port of
Dovecot's `auth-cache.c` shape), cache-key with var substitution
(`%u`/`%n`/`%d`/`%r`), `CACHE-FLUSH` command on the master socket.
Per-IP penalty / rate-limit via `internal/anvil` (`COUNTER-INC`
primitives are already in place), IPv6 masked to /48.

**Timing-leak fix (carve-out, lands here):** today an unknown
username short-circuits before any password check while a known
one runs the full bcrypt / sha512-crypt path — an attacker that
measures wall-clock auth latency can enumerate which usernames
exist. Mitigation is Dovecot's constant-time fake-compare: on
`ResultNext` (user unknown in this driver) the SQL passdb runs a
no-op password check against a dummy hash so the timing envelope
matches the verified path. Add a benchmark assertion that the two
paths' p50 latency stays within a small ratio so regressions
surface in CI.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-4.

---

## Phase AUTH-5 — additional SASL mechs

Mini-phases under one umbrella, by demand order:
`EXTERNAL` → `OAUTHBEARER` → `SCRAM-SHA-256` (+`-PLUS` channel
binding) → `CRAM-MD5` → `GSSAPI`. Each one its own PR with a
`mechanisms: [...]` config knob.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-5.

---

## Phase AUTH-6 — policy HTTP + worker pool

Async outbound POST to a configurable policy URL with JSON payload
(per Dovecot `auth-policy.c`); blocking worker pool for slow passdbs
(PAM, LDAP).

Low priority — not blocking anything. Pull when concrete need
surfaces.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-6.

---

## Phase AUTH-7 — additional passdb / userdb drivers

By operator demand: `passwd-file`, `ldap`, `lua`, `pam`, `static`,
`imap`. Order driven by ticket pressure, not pre-decided.

See [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) §Phase AUTH-7.

---

## Backend admin API — auth-aware user lookups

`/api/backend/user/info` today returns only what `backendapi` can
resolve locally: username, template-derived home, and per-namespace
existence. It does NOT call userdb — uid/gid/quota_rule/acl_groups
/director_tag/etc. are unavailable.

Blocked on: **Phase AUTH-1** above (Dovecot-style userdb-lookup
wire + `pkg/authclient`). yarilo-auth has no userdb-only lookup
today — see [docs/AUTH_REVIEW.md](docs/AUTH_REVIEW.md) for why.

After AUTH-1 lands: wire `authclient` into
`yarilo-backend-api/main.go` and enrich `handleUserInfo` with the
userdb fields exposed by AUTH-1's `UserInfo`.

---

## Backend admin API — folder write ops

`/api/backend/folder/{create,delete,rename,expunge}` are not
shipped. The IMAP path already does these correctly under ACL and
event-log discipline; the admin path would need the same wiring
(ACL check, event emission to IDLE sessions on other pods, lock
acquire).

Phase ACL-1 has landed, so the ACL piece is solved. Remaining
blocker: **Phase AUTH-1** for the admin path to know who the caller
is acting on behalf of (userdb lookup for the target user's
namespace ownership + lock owner string).

---

## mdbox — alt-storage + GUID lookup index (deferred from MDBOX-PROD-READY)

The Phase-6 mdbox stack is production-ready for typical tenants:
purge, rebuild, refcount-based O(1) COPY, binary map.index, and
multi-process Save contention are all covered. Two enhancements
remain for the heavier deployment tiers:

1. **Alt-storage tiering.** ✅ Shipped in #172. `storage.mdbox_alt_storage_path` config + `yarilo-admin backend mdbox altmove` CLI. See docs/MDBOX_ALT.md.

2. **GUID lookup index.** Fetch-by-GUID currently means scanning
   every `m.<N>` until a match (driver-level — IMAP exposes UID,
   not GUID, so the gap surfaces only through rebuild + ACL flows
   that key on GUID). Dovecot indexes GUIDs in the global map as
   an additional extension; the mdboxmap package can add a
   "guid" ext alongside the existing "map" + "ref" extensions
   without breaking on-disk compatibility. Estimate: ~200 lines.

---

## Phase OBOX-1 — object-storage mailbox backend

Sketch lives in
[memory:project_yarilo_phase_obox_backlog.md]. S3-/blob-backed
mailbox backend behind the existing `pkg/mailbox` interface, plus
the design references already captured (Stalwart, Apache James).
Deferred — no priority; standalone deployment must work first.

---

## ACL — gaps deferred from Phase ACL-1

Items intentionally scoped out of Phase ACL-1 and not yet picked up:

1. **Group identifier resolution.** `group=<name>` and
   `group-override=<name>` ACL entries parse and persist correctly,
   but `mailbox.ACL.Effective` ignores them when computing rights
   for an authenticated user. Resolving requires a yarilo-side
   group → users lookup. Dovecot reads this from userdb (`groups=`
   extra field) or from a static `passwd_groups` map; yarilo has
   neither plumbing today. Adding this means: (a) extend the
   `protocol/auth` UserdbResponse with a `Groups []string` slice,
   (b) thread groups into `*session.userInfo`, (c) extend
   `Effective` to accept a `groups []string` arg and match
   `group=` / `group-override=` entries against it. RFC 4314 §2.2.

2. **Owner-identifier match for shared / public namespaces.** PR D
   defines `isOwner` as "this is the personal namespace" — true for
   the authenticated user against their own home, false everywhere
   else. The dovecot `owner` identifier should also match when a
   user accesses a mailbox under a shared namespace whose
   filesystem owner is themselves (e.g. shared/alice/foo accessed
   by alice). Today `owner` entries on shared mailboxes match no
   one. Resolving needs a notion of "this shared mailbox is owned
   by user X", derived from the namespace's location path or a
   per-shared-mailbox owner mapping. Dovecot keeps this via the
   `mail_storage` owner field.

Each item is small in isolation but touches a different subsystem
(auth chain, namespace ownership model). Pull them in a future
ACL-EXT phase when concrete demand surfaces.

---

## Quota — RFC 9208 (Phase QUOTA-1)

Owner-paid model. Counters via `pkg/dict`. `GETQUOTAROOT` /
`SETQUOTA` IMAP commands + `/api/backend/quota/...` admin surface.
Depends on per-user storage accounting which `user/usage` already
walks today.

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

---

## mail_control_path — split control state from mail storage

Dovecot's `mail_control_path` setting puts control files
(`dovecot.index*`, `dovecot-acl`, `subscriptions`, …) into a
separate directory from the message bodies. Used when storage is
read-only / object-backed / on a different volume than control
state.

In yarilo today everything (`yarilo.index*`, soon `yarilo-acl`,
subscriptions) lives next to the message bodies under the per-folder
`indexDir`. Adding this knob means:

- `pkg/config`: new `storage.control_path` template (per-namespace
  override), defaulting to "" (= same as mail_location)
- `internal/storage/index/file`: indexDir computation splits into
  `mailDir` (bodies) + `controlDir` (index/ACL/subscriptions)
- Backends touching control-only files (fileindex, ACL backend,
  subscriptions store) read/write from controlDir, not mailDir
- Helm `values.yaml`: `storage.controlPath` value + matching PVC
  wiring (separate PV when split is requested)

No current ETA. Track here so it does not silently re-emerge as a
scope question every time we add a new control file.
