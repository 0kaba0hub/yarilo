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

## Backend admin API — session control (kick)

`yarilo-admin backend who` lists sessions (via anvil) but there is
no `kick` to terminate them. Kicking requires reaching the session
process that owns the TCP conn (`yarilo-imap`, `yarilo-pop3`,
`yarilo-submission`), which currently has no admin RPC inbound.

Unblocked by: add a tiny admin Unix socket on each session binary
(`/var/run/yarilo/<svc>-admin.sock`) speaking a one-shot TAB
protocol `KICK\t<sess-id>\n` → close that conn. Wire backend-api to
dial it on `POST /api/backend/sessions/kick`.

---

## Anvil — session TTL / heartbeat to avoid stale entries

`yarilo-anvil` registers sessions on CONNECT and drops them on
DISCONNECT. If a login pod crashes, DISCONNECT never fires and the
entry leaks until anvil restarts. `who` then over-reports.

Unblocked by: either (a) a TTL field set at CONNECT, refreshed by
periodic HEARTBEAT from login, swept by a background goroutine on
the server; or (b) push-style accounting where session binaries
(not login) keep the registration alive via keepalives.

---

## Phase LMTP-PARITY-ANVIL — close LMTP DoS gap to Dovecot parity (CRITICAL, blocks production)

The current LMTP path is materially weaker than Dovecot's
defaults. Verified against `dovecot-2.4/src/lmtp/`:

| Behaviour | Dovecot | yarilo today |
|:---|:---|:---|
| `lmtp_user_concurrency_limit` default | **10** | **0 = unlimited** |
| `0` allowed in config | **No (hard error)** | Yes (silently disables) |
| Cluster-wide concurrency | Yes (anvil LOOKUP per RCPT) | No (per-pod semaphore only) |
| LMTP visible in `who` | Yes (`service_name = "lmtp"`) | No |
| Anvil registration on RCPT | Yes (`master_service_anvil_connect`) | No |

Trivially DoS-able today: a malicious or misconfigured sender that
opens parallel deliveries against one or many recipients overruns
the per-pod semaphore (×N pods worth of parallelism), the
yarilo-locks queue, NFS inode pressure and disk space — all
silently, with zero operator visibility.

Unblocked by, matching Dovecot wire-for-wire:

1. **Anvil LOOKUP command.** Add `LOOKUP\t<user>\t<service>` →
   `OK\t<count>\n` so a client can read the cluster-wide active
   count for a (user, service) pair before deciding to accept.
2. **LMTP integration at RCPT TO.** `yarilo-lmtp` issues LOOKUP
   for the recipient, compares against
   `lmtp_user_concurrency_limit`, then `anvil.Connect` (already
   wired). Drop the per-pod `userSemaphore` once anvil owns
   accounting — single source of truth.
3. **Disconnect on every termination path.** `anvil.Disconnect`
   on DATA-end / RSET / connection drop / transaction failure.
   Mirrors Dovecot's `lmtp_local_rcpt_anvil_disconnect`.
4. **Config defaults change.** `UserConcurrencyLimit` defaults to
   10. `0` becomes a hard validation error (`"did you mean
   unlimited?"`) — exact mirror of Dovecot's check at
   `src/lmtp/lmtp-settings.c:215`.
5. **`who --protocol lmtp` now lists active deliveries.** Falls
   out for free once step 2 lands.
6. **Per-source-IP token bucket (follow-up, not blocking).** Even
   with cluster-wide per-recipient limit, a sender can fan out
   across 1M recipients. Add `RATE\t<ip>` on anvil keyed by peer
   IP with a configurable burst+rate. Optional in v1; Dovecot
   itself does not ship this — but worth adding for our threat
   model.

Why deferred from BACKEND-API-EASY: this is a wire-protocol change
to anvil (new LOOKUP command), an LMTP rework, a config default
change, and per-step tests — meaningful scope on its own. Belongs
in its own PR with a focused review.

**MUST** ship before any multi-tenant production deployment.

---

## Anvil — currently-SELECTed folder

`who` groups by user (== mailbox owner). It does NOT show the
folder a session has SELECTed because that state lives in
`internal/imap.session` and is never pushed out.

Unblocked by: session binaries push `SELECT\t<sess-id>\t<folder>`
to anvil on every IMAP SELECT/UNSELECT/EXAMINE; anvil stores it
on the SessionInfo; `who` returns it. Cost: every SELECT touches
anvil. Probably fine — SELECT is infrequent.

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

## Backend admin API — index rebuild / optimize, folder repair

`/api/backend/index/dump` is shipped (read-only). Rebuild,
optimize and folder repair are not — they require driver-specific
resync logic: maildir reconstructs the index by scanning
`cur/new/tmp`; dbox/mdbox have a different on-disk layout where
orphaned blobs have different semantics. The EASY phase
deliberately did not invent a one-size-fits-all repair API.

Unblocked by: design + implement
`mailbox.UserMailbox.Repair(folder)` per backend, then expose via
`POST /api/backend/folder/repair` and `POST /api/backend/index/{rebuild,optimize}`.

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
demand.
