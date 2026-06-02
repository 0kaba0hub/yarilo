# yarilo — deferred work

Items that are known-needed but were intentionally not shipped in
the current phase. Each entry is one paragraph: what it is, why it
was deferred, and what would unblock it.

Items get removed only when the corresponding work merges to `main`.

---

## Backend admin API — auth-aware user lookups

`/api/backend/user/info` today returns only what `backendapi` can
resolve locally: username, template-derived home, and per-namespace
existence. It does NOT call userdb — uid/gid/quota_rule/acl_groups
/director_tag/etc. are unavailable.

Unblocked by: shipping a `yarilo-auth` client wired into
`yarilo-backend-api/main.go` and a small `authclient.UserdbLookup`
helper. Then `handleUserInfo` enriches the response with the
userdb fields.

---

## Backend admin API — folder write ops

`/api/backend/folder/{create,delete,rename,expunge}` are not
shipped. The IMAP path already does these correctly under ACL and
event-log discipline; the admin path would need the same wiring
(ACL check, event emission to IDLE sessions on other pods, lock
acquire) and currently has neither.

Unblocked by: Phase ACL-1 lands first, then admin-side write
endpoints can reuse the same authorisation + event helpers.

---

## mdbox — alt-storage + GUID lookup index (deferred from MDBOX-PROD-READY)

The Phase-6 mdbox stack is production-ready for typical tenants:
purge, rebuild, refcount-based O(1) COPY, binary map.index, and
multi-process Save contention are all covered. Two enhancements
remain for the heavier deployment tiers:

1. **Alt-storage tiering.** Dovecot can migrate cold `m.<N>` files
   to slower / cheaper backing storage (the `mdbox-alt/` tree).
   yarilo has no such tiering — every body lives on the primary
   PVC. Useful once mailbox sizes outgrow the SSD budget. Needs
   per-message age detection (received-date from the dbox
   trailer), a background mover, and a Helm values knob for the
   alt-storage mount. Estimate: ~300 lines + values.yaml + tests.

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

## ACL — RFC 4314 server-side support (Phase ACL-1)

Cherry-pick upstream go-imap PR #730 (migadu), implement
`pkg/mailbox/acl.go`, `internal/imap/acl.go`, rights checks on
session ops, `yarilo-admin backend acl` CLI. Semantics already
agreed: 11 letters rev2, identifiers anyone/authenticated/owner/
user= (no group= in v1), negative rights via "-" prefix,
first-ancestor-with-explicit-ACL inheritance, owner auto-detected
in personal namespace.

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
