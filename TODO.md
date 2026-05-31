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

## Phase BACKEND-API-INDEX-OPS — rebuild / optimize / repair (maildir + dbox)

`/api/backend/index/dump` is shipped (read-only). The mutating
counterparts — `rebuild`, `optimize`, `folder repair` — are not.
They require driver-specific resync logic that does not exist on
any storage driver: the storage layer can `Save`/`Fetch`/`List`/
`Delete`, but none expose a "scan disk and regenerate index"
operation.

This phase covers **maildir** and **dbox** only. `mdbox` rebuild
lands in Phase MDBOX-PROD-READY together with the other gaps that
block mdbox from real production use.

Phase order:

1. **Add `mailbox.UserMailbox.Rebuild(folder) (Stats, error)` to
   the interface.** Implement for each driver shipped here:
   - **maildir**: walk `cur/` + `new/`, parse `:2,FLAGS` from
     filenames, read existing `yarilo-uidlist` to preserve UIDs.
   - **dbox**: scan single-message blobs; UID lives in the
     filename (`u.<N>`).
   - **mdbox**: returns `not yet implemented`; lands in
     Phase MDBOX-PROD-READY.
2. **Add `Optimize(folder)`**. For fileindex this is compacting
   `.index.log` into the base `.index` file (logic already exists
   internally, needs an exposed entry-point).
3. **Add `Repair(folder)`**. Combines rebuild + orphan cleanup +
   counter resync. Single operator-facing knob for "fix whatever
   is wrong with this folder".
4. **Admin API on top**:
   - `POST /api/backend/index/rebuild`  → `UserMailbox.Rebuild`
   - `POST /api/backend/index/optimize` → `UserMailbox.Optimize`
   - `POST /api/backend/folder/repair`  → `UserMailbox.Repair`
5. **CLI**: `yarilo-admin backend index rebuild <user> <mailbox>`,
   `... optimize ...`, `yarilo-admin backend folder repair <user> <mailbox>`.

UID semantics: **preserve** by default — `Rebuild` reads
`yarilo-uidlist` (maildir) or the per-message header (dbox) and
keeps existing UIDs intact so client UID caches stay valid
(matches Dovecot's `doveadm force-resync` default). Optional
`--reset-uids` flag for nuclear re-issue from UID=1; this bumps
UIDVALIDITY and forces clients to full resync.

---

## Phase MDBOX-PROD-READY — make mdbox safe to enable in production

mdbox today (`internal/storage/mailbox/mdbox/mdbox.go`) handles
Save / Fetch / Remove / List on single-process happy paths and
its unit tests pass. It is NOT production-safe in the current
shape — the gaps below must close before any mdbox deployment
beyond dev / staging.

### Critical — block production

1. **Purge / compaction (disk space recovery).** `Remove()` is
   lazy: it only flips `expunged=1` in `dbox.map`; the message
   bytes stay in `m.<N>` forever. There is no compaction routine.
   A busy mailbox grows `mdbox-storage/` to the sum of every
   message ever delivered, not current usage — disk fills in
   weeks. Need a `Purge()` method that rewrites `m.<N>` dropping
   expunged records and updates `dbox.map` offsets, plus a
   background trigger (Dovecot kicks compaction when expunged
   ratio crosses a threshold). Add admin endpoint
   `POST /api/backend/mdbox/purge` and CLI
   `yarilo-admin backend mdbox purge <user> [<folder>]`.
   Estimate: ~500–700 lines.

2. **Crash recovery — orphan record detection in `Rebuild()`.**
   Save writes the record to `m.<N>` first, then appends a line
   to `dbox.map`. A crash between the two leaves bytes in
   `m.<N>` invisible to any future fetch. mdbox needs a rebuild
   that scans every `m.<N>` (parse magic + size + GUID from
   headers), reconciles against `dbox.map`, and re-adds orphans
   (or expunges if the user wanted them gone — record the
   intent first). maildir avoids this via the `tmp/` → `new/`
   rename pattern; mdbox has no equivalent. Estimate: ~300–400
   lines.

3. **Persisted `next_file_id` + atomic allocation.** The current
   `currentFileID` lives in process memory; on rotation each
   process does `currentFileID++`. Re-stat under the per-mailbox
   lock protects byte writes but not file-id assignment across
   process restarts — after a pod restart the same fileID can be
   issued twice. Need a persisted `next_file_id` on disk (or a
   counter resource in `yarilo-locks`) so allocation is
   process-restart-safe. Estimate: ~150 lines + possibly a small
   `yarilo-locks` API addition.

4. **Multi-process integration tests for concurrent writes.**
   `mdbox_test.go` is single-process only. We have no test
   coverage for `yarilo-imap` and `yarilo-lmtp` writing the same
   folder simultaneously — exactly the scenario where rotation
   races, dbox.map append interleaving, and the orphan-on-crash
   edge cases bite. Estimate: ~200 lines, fixtures included.

### Important — close before scaling mdbox tenants

5. **Cross-folder GUID / refcount map for O(1) COPY.** IMAP
   COPY between folders today reads the full record and writes
   it again at a new offset, duplicating bytes. Dovecot mdbox
   keeps a global `dovecot.map.index` that maps
   `(file_id, offset) → refcount` so COPY is O(1) — one new
   entry pointing at the same bytes. Adding this is a format
   change: per-record refcount field + global map index +
   purge that respects refcount before reclaiming bytes.
   Estimate: ~700 lines + format migration.

6. **Binary `dbox.map` instead of text TSV.**
   `writeMapLines` truncates and rewrites the entire file on
   any change (`mdbox.go:538`). For a 100k-message folder every
   Remove rewrites megabytes. Dovecot uses a binary index with
   O(1) update. Estimate: ~400 lines.

7. **Alt-storage support.** Dovecot can migrate old `m.<N>`
   files to slower / cheaper storage. yarilo has no such
   tiering. Estimate: ~300 lines + Helm values.

8. **GUID lookup index.** Fetch-by-GUID currently means scanning
   every `m.<N>` until a match. Dovecot indexes GUIDs in the
   global map. Estimate: ~200 lines on top of (5).

### Why deferred together

Items 1–4 are the minimum to enable mdbox in any production at
all. 5–8 are the minimum to enable it for non-trivial tenants.
None of this is squeezable into BACKEND-API-INDEX-OPS — that
phase deliberately ships rebuild for the two drivers (maildir,
dbox) that are already production-safe; mdbox stays
`not implemented` on the rebuild endpoint until this phase
closes the gaps.

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
