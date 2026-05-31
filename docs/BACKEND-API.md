# yarilo-backend-api — backend-plane HTTP wire reference

The `yarilo-backend-api` binary exposes the operator surface over HTTP
for backend-plane operations: **dict** (today), **acl** (Phase ACL-1),
**quota** (Phase QUOTA-1), **folder / user** (future). One instance
runs per backend tag (or one per standalone deployment).

The `yarilo-admin` CLI is a thin HTTP client over this API; every
backend-plane subcommand lives under `yarilo-admin backend <service>
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
See `TODO.md`.

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

CLI: `yarilo-admin backend folder list <user> [--namespace NS]`

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

CLI: `yarilo-admin backend folder info <user> <folder>`

### `POST /api/backend/folder/guid`

Convenience extraction of `guid` from the info payload — useful for
piping into ACL / METADATA CLIs that need the GUID directly.

```json
{ "folder": "INBOX", "guid": "ab12...ef" }
```

CLI: `yarilo-admin backend folder guid <user> <folder>`

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

CLI: `yarilo-admin backend folder stats <user> <folder>`

## User endpoints

### `POST /api/backend/user/info`

Returns what backend-api can resolve locally — username, the
template-resolved home directory, every configured namespace and
whether its on-disk root exists.

It deliberately does NOT call userdb (uid/gid/quota/etc.); that
needs a yarilo-auth client which backend-api does not embed yet —
see `TODO.md` (BACKEND-API auth-aware lookups).

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
  ]
}
```

CLI: `yarilo-admin backend user info <user>`

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

CLI: `yarilo-admin backend user usage <user>`

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

CLI: `yarilo-admin backend index dump <user> <folder> [--limit N]`

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
| `mdbox` | Returns `501 Not Implemented` with a pointer to Phase MDBOX-PROD-READY (see `TODO.md`). |

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

CLI: `yarilo-admin backend index rebuild <user> <folder> [--namespace NS]`

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

CLI: `yarilo-admin backend index optimize <user> <folder> [--namespace NS]`

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

CLI: `yarilo-admin backend subscriptions {list|add|remove} <user> [<folder>] [--namespace NS]`

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

CLI: `yarilo-admin backend specialuse {list|get|set|delete} <user> [<folder>] [<attr>]`

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

CLI: `yarilo-admin backend metadata {list|get|set|delete} <user> [<folder>] --entry /private/<name> [...]`

## Who endpoint

### `POST /api/backend/who`

Active-session listing. Data source is `yarilo-anvil` — backend-api
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
and clear on `DISCONNECT`. Caveats — see [TODO.md](../TODO.md):

- LMTP does not go through anvil; LMTP deliveries are not listed.
- Stale entries can survive a login-pod crash (no TTL / heartbeat yet).
- Per-folder grouping (currently-SELECTed folder) is not tracked —
  session binaries do not push folder state into anvil yet.

Returns `501 Not Implemented` when `anvil_service.listen` is empty.

CLI: `yarilo-admin backend who [--protocol IMAP] [--user U] [--group-by user|none]`

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
yarilo-admin backend who count                          # global total
yarilo-admin backend who count imap                     # total for protocol
yarilo-admin backend who count --user alice@x.com       # total for user
yarilo-admin backend who count --by protocol            # breakdown by protocol
yarilo-admin backend who count --by user                # breakdown by user
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
| OPS-BACKEND-API (v1.23) | dict surface; `yarilo-admin backend dict` CLI as HTTP client |
| BACKEND-API-EASY (v1.24, this) | folder / user / index / subscriptions / specialuse / metadata read-write surfaces against the existing storage + dict backends |
| ACL-1 (next) | `POST /api/backend/acl/{get,set,delete,my-rights,list-rights,debug}` + `yarilo-admin backend acl` CLI |
| QUOTA-1 | `POST /api/backend/quota/{show,set,unset,recalc}` |
| BACKEND-API-AUTH | `user info` enriched with uid/gid/userdb fields via yarilo-auth RPC |
| BACKEND-API-SESSIONS | `who` / `kick` via session-binary RPC; `anvil penalties/connections` |
| BACKEND-API-WRITE | `folder create/delete/rename/expunge`, `index rebuild/optimize`, `folder repair` once driver-specific resync ships |
