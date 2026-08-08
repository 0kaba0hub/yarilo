# yarilo-backend-api — backend-plane HTTP wire reference

The `yarilo-backend-api` binary exposes the operator surface over HTTP
for backend-plane operations: **dict** (today), **acl** (Phase ACL-1),
**quota** (Phase QUOTA-1), **folder / user** (future). One instance
runs per backend tag (or one per standalone deployment).

The `yarctl` CLI is a thin HTTP client over this API; every
backend-plane subcommand lives under `yarctl backend <service>
<command>` (`backend dict ...`, future `backend acl ...`,
`backend quota ...`, etc.).

For the director's own admin endpoints (ring / backends / users /
peers) see [DIRECTOR-API.md](DIRECTOR-API.md) — different binary,
different port, different token.

---

## Transport

- **Protocol:** JSON over HTTPS (matching the existing
  `/api/director/...` surface)
- **Auth:** Bearer token in `Authorization: Bearer <token>`. Server
  reads it from the `BACKEND_API_TOKEN` env var (wired by the chart
  from a Secret). Empty token disables auth — local dev only
- **IP allow-list:** when `backend_api.allowed_nets` is set in
  `yarilo.yaml`, clients outside those CIDRs get `403 forbidden`
  before the bearer check
- **mTLS:** when `internal_tls.enabled: true`, the listener is
  TLS-terminated with the same internal CA the rest of the cluster
  uses

## Defaults

| Setting | Default |
|:---|:---|
| Listen | `:9105` |
| Auth | Bearer token, mandatory in production |
| TLS | mTLS in production; plain in dev |
| Body limit | 1 MiB per request |
| Iterate timeout | 5 minutes |
| Other op timeout | 30 seconds |

## Endpoints

### `GET /api/backend/health`

Liveness probe. Returns `200 {"status":"ok"}` whenever the process
is up. Bypasses payload constraints — usable by k8s probes.

### `GET /api/backend/dict/drivers`

Lists every dict driver registered in this process.

```json
{ "drivers": ["fail", "file", "memory", "redis", "sql"] }
```

### `GET /api/backend/dict/{name}/exists`

Reports whether the named dict is configured on this backend-api.

```json
{ "name": "metadata", "exists": true }
```

### `POST /api/backend/dict/{name}/lookup`

```json
// request
{ "key": "priv/box/<guid>/comment", "op": { "username": "alice@x.com" } }

// response
{ "found": true, "values": ["<base64>"] }
```

`op` (per-call `pkg/dict.OpSettings`) is optional. Multi-value
drivers return the full list; single-value drivers return a
one-element array. `found: false` → `values` is omitted/empty.

### `POST /api/backend/dict/{name}/iterate`

Streaming endpoint. Response `Content-Type: application/x-ndjson` —
one JSON object per line. A `{"error": "..."}` line MAY appear
mid-stream when iteration fails after some rows have been emitted;
clients MUST check every line for the `error` key.

```json
// request
{
  "path": "priv/box/",
  "flags": 3,
  "op": { "username": "alice@x.com" }
}

// response (NDJSON, one row per line)
{"key":"priv/box/abc123/comment","values":["<base64>"]}
{"key":"priv/box/abc123/admin","values":["<base64>"]}
```

**Flags bitmask** (`pkg/dict.IterFlag`):

| Bit | Value | Meaning |
|:---|:---|:---|
| 0 | 1 | `Recurse` — descend into sub-hierarchies |
| 1 | 2 | `SortByKey` |
| 2 | 4 | `SortByValue` |
| 3 | 8 | `NoValue` — omit values from rows |
| 4 | 16 | `ExactKey` — return all values for one exact key (no recursion) |

### `POST /api/backend/dict/{name}/set`

```json
// request
{ "key": "priv/foo", "value": "<base64>", "op": {} }

// response
{ "result": 1, "status": "ok" }
```

`result` is the raw `pkg/dict.CommitResult` value (`1` = OK,
`0` = not-found, `-1` = failed, `-2` = write-uncertain). `status`
is the human-readable mirror used by the CLI.

### `POST /api/backend/dict/{name}/unset`

```json
// request
{ "key": "priv/foo", "op": {} }

// response
{ "result": 1, "status": "ok" }
```

Unsetting a missing key is not an error — `status: ok`.

### `POST /api/backend/dict/{name}/atomic-inc`

```json
// request
{ "key": "priv/quota/storage", "delta": 1024, "op": {} }

// response when key exists
{ "result": 1, "status": "ok" }

// response when key is missing
{ "result": 0, "status": "not-found" }
```

### `POST /api/backend/dict/{name}/expire-scan`

```json
// request
{}

// response
{ "status": "ok" }
```

Drivers without TTL support are a no-op (still 200).

### `POST /api/backend/dict/{name}/commit-batch`

Multi-op atomic transaction. Returns a single commit result; on
failure no individual op is applied.

```json
// request
{
  "op": { "username": "alice@x.com" },
  "ops": [
    { "kind": "set",        "key": "a", "value": "<base64>" },
    { "kind": "unset",      "key": "b" },
    { "kind": "atomic-inc", "key": "counter", "delta": 5 }
  ]
}

// response
{ "result": 1, "status": "ok" }
```

`kind` is one of `set` / `unset` / `atomic-inc`.

## Error format

Errors come back with the matching HTTP status and a JSON body:

```json
{ "error": "dict \"no-such\" not configured" }
```

| Status | Meaning |
|:---|:---|
| 400 | bad request body / malformed JSON / unknown driver |
| 401 | missing or invalid bearer token |
| 403 | client IP not in `allowed_nets` |
| 404 | dict name not configured on this backend-api |
| 500 | driver / I/O error |
| 503 | dict closed (process shutting down) |

## Folder endpoints

Read-only inspection of mailbox state. Mutating folder ops
(create / delete / rename / expunge) are deferred — they need
IMAP-level ACL context and proper event emission to live sessions.
Deferred (tracked in the internal backlog).

Common request body (every folder endpoint accepts it):

```json
{ "user": "alice@x.com", "folder": "INBOX", "namespace": "personal" }
```

`namespace` defaults to `personal` when omitted. Other valid values
are the slugs configured under `namespaces[].prefix` (e.g.
`shared`, `public`).

### `POST /api/backend/folder/list`

Returns every folder visible in the namespace via the underlying
storage driver (`UserMailbox.ListFolders`).

```json
{ "folders": ["INBOX", "Sent", "Trash"] }
```

CLI: `yarctl backend folder list <user> [--namespace NS]`

> **`NS` is the namespace slug, taken from its prefix, not from its type.** A
> namespace declared `type: shared` with `prefix: "Public/"` is addressed as
> `--namespace public`; `--namespace shared` reports that no such namespace is
> configured. Both readings are natural, which is why it is stated here.
>
> **The namespace root** — the ACL a shared namespace needs before anyone can
> create a mailbox in it — is addressed with `--root` on the CLI and
> `"root": true` on the wire, never by omitting the folder. Folder is required
> everywhere else, so a dropped argument fails instead of becoming a grant on
> the whole namespace.
>
> **A named folder must exist.** `get`, `set` and `delete` answer `404 folder
> not found` for a mailbox that is not there, matching what the IMAP ACL
> commands have answered since 2.3.62. Previously a misspelt name was written
> as given, and the store created the directory on the way — a typo became a
> mailbox with permissions and no messages. `--root` names no folder, so it is
> not affected.
>
> **`apply` changes one entry atomically.** `POST /api/backend/acl/apply`
> (`yarctl backend acl set`) modifies a single identifier — `"mode": "add" |
> "remove" | "replace"`, default replace, empty rights with replace removes the
> entry (RFC 4314 §3.1). The read-modify-write runs on the server inside the
> folder lock, so a concurrent IMAP `SETACL` between the read and the write
> cannot be lost. The CLI used to `get` the whole ACL, edit it and `set` it back
> across two unlocked calls, which lost a concurrent write and made the client
> own the canonical identifier form; `set`/`delete` of a single identifier now
> route through `apply`. Full-ACL replace stays on `/acl/set` for callers that
> genuinely mean to write the whole file.
>
> **`materialise` repairs inheritance.** `POST /api/backend/acl/materialise`
> (`yarctl backend acl materialise <user> <folder>…`) writes what each mailbox
> inherits into its own ACL, for mailboxes created before inheritance was
> materialised at creation. It is a **dry run unless `"apply": true`**, it only
> ever adds — an entry already in the file is left exactly as it is and reported
> under `skipped` — and a second run adds nothing. It is not automatic on
> purpose: a mailbox orphaned by the old rule and one whose ACL deliberately
> omits an identifier are the same file on disk. Both lists name the rights as
> well as the identifier — the operator is being asked to tell a repair from a
> widening, and bare identifiers print the two the same. The mailboxes are named
> explicitly; there is no namespace-wide sweep, so widening a whole namespace is
> not one keystroke.
>
> **`rebuild` is the exception, deliberately.** It reseeds the index from files
> already on disk, creates nothing, and is run precisely when the state is
> already inconsistent — refusing the whole batch over one stale name would
> fail on the state it repairs. It reports instead: `folders` counts the
> mailboxes actually reseeded, and `skipped` lists the rest with a reason
> (`folder not found` or `no ACL`). A batch whose names are all typos answers
> `200` with an empty `rebuilt`, which is the honest answer to "reseed nothing".
>
> **`rebuild` merges; `rebuild --all` replaces.** A named folder list reseeds
> exactly those folders and leaves every other folder's index rows untouched —
> repairing one mailbox must not delete the index for the rest (#1151). Passing
> `"all": true` (CLI `--all`) instead addresses every folder in the namespace and
> *replaces* the index, which is the only form that clears rows for folders that
> no longer exist. `all` and `folders` are alternatives (`400` together), and the
> reply echoes `"all"` so the mode that ran is visible. `--all` is also the answer
> to drift the operator cannot enumerate — the drifted index was what would have
> told them which folders to name.
>
> **`--all` includes the namespace root, and reports it separately.** The root
> carries its own ACL (`yarilo-acl-root`) and its own index rows, and it is not a
> folder — no folder listing contains it. A replace built from folders alone would
> delete the bootstrap grant a shared namespace cannot work without, so `--all`
> addresses the root explicitly. In the reply, `rebuilt` lists **folders** (and
> `folders` counts them) while the index was replaced from *all of them plus the
> root*; the root's own outcome is the `root` boolean — `false` there means the
> root simply holds no ACL, never "not found", since it has no name to look up.
> A namespace with no folders is not a special case: `--all` then hands the
> replace a genuinely complete set (the root alone) and every other row is
> cleared, which is the maximal orphan case and precisely the repair. Blanking on
> a *failed* enumeration would be the hazard, and that answers `500` first.

### `POST /api/backend/subscriptions/migrate`

Folds a namespace's old per-namespace subscription file into the subscriber's
own, for a namespace that no longer keeps one (the default for every
non-personal namespace unless `subscriptions: true` is set on a fixed
shared/public one; always so for an owner-templated namespace). Dry run unless
`"apply": true`.

Those rows were written into the **owner's** store, and every one names a mailbox
in the owner's own space, so folding restores the owner's subscriptions exactly;
the owner also inherits any a peer created, all pointing at mailboxes they
already see. Authorship was never recorded, so a peer's subscription cannot be
returned to the peer — peers re-subscribe themselves. Deleting the file instead
would have removed the owner's own subscriptions silently.

Idempotent: the sources are removed only after every row is in the destination,
so a failure leaves the run repeatable, and a second run finds nothing. Both
historical names are read — the current one and the pre-#1159 path form.

Reply: `sources` (files read), `folded` (keys added), `already` (keys the
destination held).

CLI: `yarctl backend subscriptions migrate <user> --namespace NS [--apply]`

### `POST /api/backend/folder/info`

Folder metadata. `guid` is the 16-byte rename-stable identifier
stamped at folder creation — survives RENAME and is used as the
ACL/metadata key namespace.

```json
{
  "name":           "INBOX",
  "guid":           "ab12...ef",
  "uid_validity":   1747000000,
  "next_uid":       42,
  "messages":       7,
  "unseen":         3,
  "highest_modseq": 19
}
```

CLI: `yarctl backend folder info <user> <folder>`

### `POST /api/backend/folder/guid`

Convenience extraction of `guid` from the info payload — useful for
piping into ACL / METADATA CLIs that need the GUID directly.

```json
{ "folder": "INBOX", "guid": "ab12...ef" }
```

CLI: `yarctl backend folder guid <user> <folder>`

### `POST /api/backend/folder/stats`

`folder/info` plus an on-disk rollup (sum of physical message
sizes from `UserMailbox.List`).

```json
{
  "name":            "INBOX",
  "guid":            "ab12...ef",
  "uid_validity":    1747000000,
  "next_uid":        42,
  "messages":        7,
  "unseen":          3,
  "highest_modseq":  19,
  "size_bytes":      1234567,
  "on_disk_count":   7
}
```

CLI: `yarctl backend folder stats <user> <folder>`

### `POST /api/backend/folder/create`

Creates a new folder under the named namespace. When `special_use`
is set AND the namespace is `personal`, the folder is registered
with that RFC 6154 attribute via the special-use store so a
subsequent `LIST` surfaces it (matches the IMAP CREATE-SPECIAL-USE
flow). 409 when the folder already exists.

```json
{
  "user":        "alice@x.com",
  "folder":      "Archive",
  "namespace":   "personal",
  "special_use": "\\Archive"
}
```

Response (200):

```json
{ "status": "ok" }
```

When the folder is created but `special_use` registration fails,
the response is 200 with a `special_use_error` field — the folder
exists, the operator just needs to follow up to set the attr.

**Authorisation:** admin path — bypasses ACL. See the security
note at the top of this document; backend-api is gated by
`Token`, `AllowedNets`, and mTLS.

CLI: `yarctl backend folder create <user> <folder> [--namespace NS] [--special-use ATTR]`

### `POST /api/backend/folder/delete`

Removes a folder, its index state, and any `yarilo-acl` file +
namespace-wide list entries pointing at it. The mailbox blob
storage handles the on-disk teardown; per-mailbox ACL cleanup is
non-fatal — when it fails the operation still returns 200 with a
warning logged so the admin can correlate.

```json
{ "user": "alice@x.com", "folder": "Old", "namespace": "personal" }
```

Response (200): `{ "status": "ok" }`. 404 on missing folder.

CLI: `yarctl backend folder delete <user> <folder> [--namespace NS]`

### `POST /api/backend/folder/rename`

Renames a folder within one namespace (cross-namespace rename is
not supported). The `yarilo-acl` file is moved across index dirs
and the namespace-wide index entries are rewritten. INBOX cannot
be renamed via backend-api (reference-style move-messages semantics
not implemented here yet).

```json
{
  "user":       "alice@x.com",
  "old_folder": "Drafts",
  "new_folder": "OldDrafts",
  "namespace":  "personal"
}
```

Response (200): `{ "status": "ok" }`. 404 on missing source, 409 on
destination conflict, 400 when `old_folder == "INBOX"`.

CLI: `yarctl backend folder rename <user> <old> <new> [--namespace NS]`

### `POST /api/backend/folder/expunge`

Removes every message currently flagged `\Deleted` from a folder
(matches the IMAP EXPUNGE semantic). When `uids` is set, only
those specific UIDs are considered (matches UID EXPUNGE / UIDPLUS).
Returns the expunged UID list plus the count.

```json
{
  "user":      "alice@x.com",
  "folder":    "Trash",
  "namespace": "personal",
  "uids":      [42, 43]
}
```

Response (200):

```json
{ "status": "ok", "expunged": [42, 43], "count": 2 }
```

404 on missing folder. Per-message removal errors are logged at
Warn and the operation continues with the next message — partial
expunge is surfaced via the returned `expunged` list (count may be
smaller than requested).

CLI: `yarctl backend folder expunge <user> <folder> [--namespace NS] [--uids 1,2,3]`

## User endpoints

### `POST /api/backend/user/info`

Returns what backend-api can resolve locally — username, the
template-resolved home directory, every configured namespace and
whether its on-disk root exists — plus the userdb block when
`backend_api.auth_master_addr` is configured (Phase AUTH-1).

The userdb block is rendered as a nested object so the local view
is never mixed with the auth-side view; `userdb_status` carries the
terminal state of the call (`"ok"` / `"not_found"` / `"error"`).

```json
{
  "username": "alice@x.com",
  "home":     "/var/mail/vhosts/x.com/alice",
  "namespaces": [
    {
      "name":     "personal",
      "type":     "personal",
      "prefix":   "",
      "home":     "/var/mail/vhosts/x.com/alice",
      "location": "",
      "exists":   true
    }
  ],
  "userdb_status": "ok",
  "userdb": {
    "uid":           1001,
    "gid":           1001,
    "home":          "/var/mail/vhosts/x.com/alice",
    "mail_location": "maildir:~/Maildir",
    "groups":        ["staff", "mail"],
    "quota_rule":    ["*:storage=5G"],
    "allow_nets":    ["10.0.0.0/8"],
    "extra":         { "tier": "gold" }
  }
}
```

When `auth_master_addr` is unset, the `userdb` and `userdb_status`
keys are absent — admin tools that only care about the local view
get the pre-AUTH-1 response shape unchanged. When the master-protocol
call fails (auth down, network), the response still returns 200 with
`userdb_status: "error"` and the `userdb` key set to `null`, so admin
tooling that values the local view is not blocked by auth-side
flakiness.

CLI: `yarctl backend user info <user>`

### `POST /api/backend/user/iterate`

Enumerates every username the yarilo-auth userdb backend can
surface. Thin wrapper over `pkg/authclient`'s `IterateUsers`; the
response is a sorted username array.

```json
{ "users": ["alice@x.com", "bob@x.com", "carol@x.com"] }
```

Returns 503 when `backend_api.auth_master_addr` is unset (no userdb
to enumerate); returns 502 when the master-protocol call fails
(reason text in the JSON `error` field).

CLI: `yarctl backend user iterate`

### `POST /api/backend/user/usage`

Walks every folder in every implemented namespace and reports
per-folder message + byte totals plus the rollups. Suitable for
ad-hoc capacity inspection before QUOTA-1 ships.

```json
{
  "user": "alice@x.com",
  "folders": [
    { "namespace": "personal", "folder": "INBOX", "messages": 7, "size_bytes": 1234567 }
  ],
  "total_messages":   7,
  "total_size_bytes": 1234567
}
```

CLI: `yarctl backend user usage <user>`

## Index endpoints

### `POST /api/backend/index/dump`

Walks an existing folder's fileindex and returns every record.
Use the optional `limit` field to cap the response size.

```json
// request
{ "user": "alice@x.com", "folder": "INBOX", "namespace": "personal", "limit": 100 }

// response
{
  "folder":         "INBOX",
  "folder_guid":    "ab12...ef",
  "uid_validity":   1747000000,
  "next_uid":       42,
  "highest_modseq": 19,
  "truncated":      false,
  "records": [
    { "uid": 1, "filename": "1747000000.M...", "flags": ["\\Seen"], "modseq": 5, "size": 1234, "vsize": 1234 }
  ]
}
```

CLI: `yarctl backend index dump <user> <folder> [--limit N]`

### `POST /api/backend/index/rebuild`

Regenerates the fileindex for one folder from the on-disk storage
(driver-specific `Scan`). The new index preserves every UID that
the old index already knew for the same filename and assigns fresh
UIDs (from the current `next_uid`) to filenames the index has not
seen — so client UID caches stay valid for everything they could
already see.

Driver support:

| Driver | Behaviour |
|:---|:---|
| `maildir` | Walks `cur/` + `new/`, parses flags + size from filename. Flags from disk win over the previous index (the filename is the source of truth for maildir). |
| `dbox` | Walks `u.<seq>` files, reads GUID + size + Received date from the per-file trailer. Flags are left empty in the scan — the rebuild keeps prior index flags. |
| `mdbox` | Returns `501 Not Implemented` with a pointer to Phase MDBOX-PROD-READY (deferred). |

```json
// request
{ "user": "alice@x.com", "folder": "INBOX", "namespace": "personal" }

// response
{
  "folder":           "INBOX",
  "folder_guid":      "ab12...ef",
  "scanned":          42,
  "uids_preserved":   40,
  "uids_assigned":    2,
  "orphans_dropped":  0,
  "duration_ms":      37
}
```

Optional `"reset_uids": true` is rejected with `501` today —
nuking UIDs forces every client to full resync via UIDVALIDITY
bump, so the v1 path is `DELETE + CREATE` via IMAP. Will land
once the design for UIDVALIDITY semantics is locked.

The endpoint takes the cross-process mailbox lock
(`locks.MailboxKey`) for the whole rebuild so concurrent
IMAP writers cannot race the snapshot.

CLI: `yarctl backend index rebuild <user> <folder> [--namespace NS]`

### `POST /api/backend/index/optimize`

Compacts the `.index.log` overlay into the base `.index` file.
No semantic change — records, UIDs, modseq stay identical; only
the on-disk layout shrinks. Fast no-op when the log already only
contains its header.

```json
// request
{ "user": "alice@x.com", "folder": "INBOX", "namespace": "personal" }

// response
{ "folder": "INBOX", "duration_ms": 4 }
```

CLI: `yarctl backend index optimize <user> <folder> [--namespace NS]`

## Subscriptions endpoints

Per-user IMAP SUBSCRIBE state. Reuses
`internal/userstate/subs.Store` — same on-disk format (sorted folder
names, tmp+rename atomicity) and the same `locks.SubscriptionsKey`
as IMAP, so concurrent sessions see admin writes immediately.

| Endpoint | Request | Response |
|:---|:---|:---|
| `POST /api/backend/subscriptions/list` | `{user, namespace?}` | `{"subscriptions": [...]}` |
| `POST /api/backend/subscriptions/add` | `{user, folder, namespace?}` | `{"status": "ok"}` |
| `POST /api/backend/subscriptions/remove` | `{user, folder, namespace?}` | `{"status": "ok"}` |

CLI: `yarctl backend subscriptions {list|add|remove} <user> [<folder>] [--namespace NS]`

## SpecialUse endpoints

Per-user RFC 6154 special-use overrides. Reuses
`internal/userstate/specialuse.Store` — same on-disk format and
the same lock key as IMAP `CREATE (USE ...)`.

Only the personal namespace carries special-use — RFC 6154
`\Sent` / `\Drafts` / etc. do not extend to shared or public.

| Endpoint | Request | Response |
|:---|:---|:---|
| `POST /api/backend/specialuse/list` | `{user}` | `{"overrides": {...}, "defaults": {...}}` |
| `POST /api/backend/specialuse/get` | `{user, folder}` | `{"folder", "attr", "source": "override"\|"default"\|"none"}` |
| `POST /api/backend/specialuse/set` | `{user, folder, attr}` | `{"status": "ok"}` |
| `POST /api/backend/specialuse/delete` | `{user, folder}` | `{"status": "ok"}` |

CLI: `yarctl backend specialuse {list|get|set|delete} <user> [<folder>] [<attr>]`

## Metadata endpoints

RFC 5464 METADATA admin surface backed by the configured `metadata`
dict (same one IMAP GETMETADATA / SETMETADATA reads/writes). Keys
follow the GUID-namespaced layout from `pkg/mailbox/attribute.go`,
so admin writes are visible to the next IMAP round-trip.

Request envelope (every metadata endpoint accepts it):

```json
{
  "user":      "alice@x.com",
  "folder":    "INBOX",
  "namespace": "personal",
  "scope":     "private",
  "entry":     "/private/comment",
  "value":     "<base64>",
  "as_user":   "alice@x.com"
}
```

- Empty `folder` targets server scope (vendor-prefixed under INBOX's GUID).
- `scope` is `private` or `shared` for `list`; `get`/`set`/`delete`
  derive it from the leading `/private/` or `/shared/` in `entry`.
- `as_user` matters for shared/public folders under `/private/`
  scope where each user has their own slice; defaults to `user`.

| Endpoint | Notes |
|:---|:---|
| `POST /api/backend/metadata/list` | Iterates every entry under the chosen scope; values base64-encoded. |
| `POST /api/backend/metadata/get` | Returns `{found, value}` for one entry. |
| `POST /api/backend/metadata/set` | `value` is base64. Wraps a single dict transaction. |
| `POST /api/backend/metadata/delete` | Unset one entry under one dict transaction. |

CLI: `yarctl backend metadata {list|get|set|delete} <user> [<folder>] --entry /private/<name> [...]`

## Who endpoint

### `POST /api/backend/who`

Active-session listing. Data source is `yarilo-warden` — backend-api
dials it per request, runs `WHO`, then closes.

```json
// request (all fields optional)
{ "service": "imap", "user": "alice@x.com", "group_by": "user" }

// response (default group_by="user")
{
  "total": 2,
  "groups": [
    {
      "user":  "alice@example.com",
      "total": 1,
      "sessions": [
        { "id": "s1", "user": "alice@example.com", "ip": "1.1.1.1", "service": "imap", "connected_at": "2026-05-31T15:00:00Z" }
      ]
    }
  ]
}

// response when group_by="none"
{ "total": 2, "sessions": [ ... flat list ... ] }
```

Filters: `service=imap|pop3|submission|lmtp` and `user=<exact>`.

**What "active" means:** entries register on login-pod `CONNECT`
and clear on `DISCONNECT`. Caveats:

- LMTP does not go through warden; LMTP deliveries are not listed.
- Stale entries can survive a login-pod crash (no TTL / heartbeat yet).
- Per-folder grouping (currently-SELECTed folder) is not tracked —
  session binaries do not push folder state into warden yet.

Returns `501 Not Implemented` when `warden_service.listen` is empty.

CLI: `yarctl backend who [--protocol IMAP] [--user U] [--group-by user|none]`

### `POST /api/backend/who/count`

Aggregated counts. Same filters as `/who` plus an optional
breakdown dimension.

```json
// request
{ "service": "imap", "user": "alice@x.com", "by": "" }

// response
{ "total": 1, "service": "imap", "user": "alice@x.com" }

// request — breakdown by protocol
{ "by": "protocol" }

// response
{
  "total":       5,
  "by_protocol": { "imap": 3, "pop3": 1, "submission": 1 }
}

// request — breakdown by user
{ "by": "user" }

// response
{
  "total":   5,
  "by_user": { "alice@x.com": 2, "bob@x.com": 3 }
}
```

CLI:

```
yarctl backend who count                          # global total
yarctl backend who count imap                     # total for protocol
yarctl backend who count --user alice@x.com       # total for user
yarctl backend who count --by protocol            # breakdown by protocol
yarctl backend who count --by user                # breakdown by user
```

## OpSettings shape

Used in the `op` field of every endpoint that mutates or reads
per-user state. All fields optional; an empty `op` is equivalent
to no `op` field at all.

```json
{
  "username": "alice@example.com",
  "home_dir": "/var/mail/vhosts/example.com/alice",
  "expire_secs": 3600
}
```

## Phase roadmap

| Phase | Adds |
|:---|:---|
| OPS-BACKEND-API (v1.23) | dict surface; `yarctl backend dict` CLI as HTTP client |
| BACKEND-API-EASY (v1.24, this) | folder / user / index / subscriptions / specialuse / metadata read-write surfaces against the existing storage + dict backends |
| ACL-1 (next) | `POST /api/backend/acl/{get,set,delete,my-rights,list-rights,debug}` + `yarctl backend acl` CLI |
| QUOTA-1 | `POST /api/backend/quota/{show,set,unset,recalc}` |
| BACKEND-API-AUTH | `user info` enriched with uid/gid/userdb fields via yarilo-auth RPC |
| BACKEND-API-SESSIONS | `who` / `kick` via session-binary RPC; `warden penalties/connections` |
| BACKEND-API-WRITE | `folder create/delete/rename/expunge`, `index rebuild/optimize`, `folder repair` once driver-specific resync ships |
