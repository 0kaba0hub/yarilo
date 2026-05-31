# yarilo-admin-api — storage-plane HTTP wire reference

The `yarilo-admin-api` binary exposes the operator surface over HTTP
for storage-plane operations: **dict** (today), **acl** (Phase ACL-1),
**quota** (Phase QUOTA-1), **folder / user** (future). One instance
runs per backend tag (or one per standalone deployment).

The `yarilo-admin` CLI is a thin HTTP client over this API. Future
phases route the same wire calls through the director's
`/api/admin/proxy/<user>/...` when multi-backend deployment lands —
no CLI change.

For the director's own admin endpoints (ring / backends / users /
peers) see [DIRECTOR-API.md](DIRECTOR-API.md) — different binary,
different port, different token.

---

## Transport

- **Protocol:** JSON over HTTPS (matching the existing
  `/api/director/...` surface)
- **Auth:** Bearer token in `Authorization: Bearer <token>`. Server
  reads it from the `ADMIN_API_TOKEN` env var (wired by the chart
  from a Secret). Empty token disables auth — local dev only
- **IP allow-list:** when `admin_api.allowed_nets` is set in
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

### `GET /api/admin/health`

Liveness probe. Returns `200 {"status":"ok"}` whenever the process
is up. Bypasses payload constraints — usable by k8s probes.

### `GET /api/admin/dict/drivers`

Lists every dict driver registered in this process.

```json
{ "drivers": ["fail", "file", "memory", "redis", "sql"] }
```

### `GET /api/admin/dict/{name}/exists`

Reports whether the named dict is configured on this admin-api.

```json
{ "name": "metadata", "exists": true }
```

### `POST /api/admin/dict/{name}/lookup`

```json
// request
{ "key": "priv/box/<guid>/comment", "op": { "username": "alice@x.com" } }

// response
{ "found": true, "values": ["<base64>"] }
```

`op` (per-call `pkg/dict.OpSettings`) is optional. Multi-value
drivers return the full list; single-value drivers return a
one-element array. `found: false` → `values` is omitted/empty.

### `POST /api/admin/dict/{name}/iterate`

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

### `POST /api/admin/dict/{name}/set`

```json
// request
{ "key": "priv/foo", "value": "<base64>", "op": {} }

// response
{ "result": 1, "status": "ok" }
```

`result` is the raw `pkg/dict.CommitResult` value (`1` = OK,
`0` = not-found, `-1` = failed, `-2` = write-uncertain). `status`
is the human-readable mirror used by the CLI.

### `POST /api/admin/dict/{name}/unset`

```json
// request
{ "key": "priv/foo", "op": {} }

// response
{ "result": 1, "status": "ok" }
```

Unsetting a missing key is not an error — `status: ok`.

### `POST /api/admin/dict/{name}/atomic-inc`

```json
// request
{ "key": "priv/quota/storage", "delta": 1024, "op": {} }

// response when key exists
{ "result": 1, "status": "ok" }

// response when key is missing
{ "result": 0, "status": "not-found" }
```

### `POST /api/admin/dict/{name}/expire-scan`

```json
// request
{}

// response
{ "status": "ok" }
```

Drivers without TTL support are a no-op (still 200).

### `POST /api/admin/dict/{name}/commit-batch`

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
| 404 | dict name not configured on this admin-api |
| 500 | driver / I/O error |
| 503 | dict closed (process shutting down) |

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
| OPS-ADMIN-API (this, v1.23) | dict surface above; `yarilo-admin dict` CLI as HTTP client |
| ACL-1 (next) | `POST /api/admin/acl/{user}/{mailbox}/{get,set,delete,my-rights,list-rights,debug}` + `yarilo-admin acl` CLI |
| QUOTA-1 | `POST /api/admin/quota/{user}/{show,set,unset,recalc}` |
| Folder ops | `POST /api/admin/folder/{user}/{list,info,guid}` for mailbox→GUID lookups |
| OPS-ADMIN-PROXY | director's `/api/admin/proxy/{user}/{rest...}` transparently proxies to the right backend's admin-api by ring lookup |
