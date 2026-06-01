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
