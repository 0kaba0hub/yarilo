# yarilo-director HTTP admin API

Director-plane admin endpoints exposed by `yarilo-director` on port
`9103` (default). All require a Bearer token and an IP allow-list.

For the storage-plane admin API (dict / acl / quota / folder),
see [BACKEND-API.md](BACKEND-API.md) — different binary
(`yarilo-backend-api`), different port (`:9105`), different token.

Both are reachable from the same `yarilo-admin` CLI — director ops
via `--url` / `--token`, storage ops via `--backend-url` /
`--backend-token`.

---

## Authentication

Every request must include:

```
Authorization: Bearer <token>
```

The token is auto-generated into the k8s Secret `<release>-director-api-token` on first
Helm install. To read it:

```sh
kubectl get secret yarilo-director-api-token -o jsonpath='{.data.token}' | base64 -d
```

To rotate: delete the Secret and run `helm upgrade`.

---

## IP Whitelist

Default allowed CIDRs (configurable via `components.director.api.allowedNets`):

| CIDR | Purpose |
|:---|:---|
| `127.0.0.0/8` | Loopback — same-pod CLI |
| `10.96.0.0/12` | k8s service CIDR (kubeadm default) |
| `10.244.0.0/16` | k8s pod CIDR (flannel/kubeadm default) |

---

## CLI

`yarilo-admin` runs inside the director pod and requires no flags — reads URL and token
from environment automatically:

```sh
kubectl exec -it <director-pod> -- yarilo-admin director status
```

Environment variables (set automatically in the container):

| Variable | Default | Description |
|:---|:---|:---|
| `YARILO_ADMIN_URL` | `http://localhost:9103` | API base URL |
| `YARILO_ADMIN_TOKEN` | — | Bearer token (fallback: `DIRECTOR_API_TOKEN`) |

---

## Endpoints

### Status & Diagnostics

#### `GET /api/director/status`

Ring state overview: all backends and peer directors.

```json
{
  "backends": [
    {"ip": "10.0.0.1", "port": 993, "tag": "ssd", "up": true, "vhosts": 100}
  ],
  "peers": ["10.0.0.2:9102"]
}
```

CLI: `yarilo-admin director status`

---

#### `GET /api/director/dump`

Full state dump: backends, active user→backend mappings, peers.

```json
{
  "backends": [
    {"ip": "10.0.0.1", "port": 993, "tag": "ssd", "up": true, "vhosts": 100, "last_updown_change": 1747000000}
  ],
  "users": [
    {"hash": 3141592653, "host": "10.0.0.1:993", "weak": false, "expires_at": 1747001800}
  ],
  "peers": ["10.0.0.2:9102"]
}
```

CLI: `yarilo-admin director dump`

---

#### `GET /api/director/map[?user=USER]`

Without `user` — returns all active user→backend entries from the director's userDir.
With `user` — performs a live ring lookup for that username.

```json
// GET /api/director/map?user=alice@example.com
{"user": "alice@example.com", "backend": "10.0.0.1", "port": 993, "tag": "ssd"}

// GET /api/director/map
{"users": [{"hash": 3141592653, "host": "10.0.0.1:993", "weak": false}]}
```

CLI: `yarilo-admin director map [--user alice@example.com]`

---

### Backends

#### `GET /api/director/backends`

List all backends currently in the ring.

```json
{"backends": [...]}
```

CLI: `yarilo-admin director backends list`

---

#### `POST /api/director/backends`

Add a backend to the ring. Broadcasts `RING-CHANGE up` to all connected directors.

```json
// Request
{"ip": "10.0.0.3", "port": 993, "tag": "ssd", "vhosts": 100}

// Response
{"status": "ok"}
```

CLI: `yarilo-admin director backends add 10.0.0.3 --port 993 --tag ssd`

---

#### `PATCH /api/director/backends/{ip}`

Update virtual node weight of an existing backend.

```json
// Request
{"vhosts": 200}

// Response
{"status": "ok"}
```

CLI: `yarilo-admin director backends update 10.0.0.3 --vhosts 200`

---

#### `DELETE /api/director/backends/{ip}`

Remove backend from the ring. Broadcasts `RING-CHANGE down`.

CLI: `yarilo-admin director backends remove 10.0.0.3`

---

#### `POST /api/director/backends/{ip}/up`

Mark backend as up (resumes new session routing). Broadcasts `RING-CHANGE up`.

CLI: `yarilo-admin director backends up 10.0.0.3`

---

#### `POST /api/director/backends/{ip}/down`

Mark backend as down (flush — stops new routing, keeps in registry). Broadcasts `RING-CHANGE flush`.

CLI: `yarilo-admin director backends down 10.0.0.3`

---

#### `POST /api/director/backends/{ip}/flush`

Flush a specific backend or all backends. Use `all` as `{ip}` to flush everything.

```sh
POST /api/director/backends/10.0.0.3/flush
POST /api/director/backends/all/flush
```

CLI: `yarilo-admin director backends flush 10.0.0.3`
CLI: `yarilo-admin director backends flush all`

---

### Users

#### `POST /api/director/users/{user}/move`

Force-assign a user to a specific backend. Overrides consistent-hash routing.
Broadcasts `USER-MOVED` to all connected directors.

```json
// Request — either form works:
{"backend": "10.0.0.1:993"}
{"ip": "10.0.0.1", "port": 993}

// Response
{"status": "ok"}
```

CLI: `yarilo-admin director users move alice@example.com --backend 10.0.0.1:993`

---

#### `POST /api/director/users/{user}/kick`

Kick a user — broadcasts `USER-KICKED` to all connected login clients, which terminate
active sessions for that user.

CLI: `yarilo-admin director users kick alice@example.com`

---

### Ring (Director Peers)

#### `GET /api/director/ring`

Ring topology as **this replica** sees it (membership is per-replica). Each
member carries its computed `left`/`right` neighbors (`(ip,port)` order; `null`
at N=1). `link` is present only for this replica's direct neighbors and
describes the live edge — `role` (`left`/`right`, or `both` at N=2 where one
connection serves both directions), `state` (`connected`/`reconnecting`) and
`since` (RFC3339, `null` while reconnecting). `seq` is the dedup watermark
(highest seq processed from that origin; `null` when none heard). `tombstones`
lists members known dead on this replica with the tombstone age.

```json
{
  "schemaVersion": 1,
  "self": "10.0.0.2:9102",
  "size": 3,
  "members": [
    {"addr": "10.0.0.1:9102", "index": 0, "self": false, "left": "10.0.0.3:9102", "right": "10.0.0.2:9102", "seq": 41, "link": {"role": "left", "state": "connected", "since": "2026-07-27T09:56:42Z"}},
    {"addr": "10.0.0.2:9102", "index": 1, "self": true,  "left": "10.0.0.1:9102", "right": "10.0.0.3:9102", "seq": 42, "link": null},
    {"addr": "10.0.0.3:9102", "index": 2, "self": false, "left": "10.0.0.2:9102", "right": "10.0.0.1:9102", "seq": 40, "link": {"role": "right", "state": "connected", "since": "2026-07-27T09:56:42Z"}}
  ],
  "tombstones": []
}
```

CLI: `yarilo-admin director ring status`

---

#### `POST /api/director/ring`

Dynamically add a peer director. Starts a persistent reconnecting dial loop.

```json
// Request
{"addr": "10.0.0.4:9102"}

// Response
{"status": "ok"}
```

> **Note:** Dynamic peers are not persisted — they are lost on pod restart.
> For permanent peers, set `components.director.peers` in Helm values.

CLI: `yarilo-admin director ring add 10.0.0.4:9102`

---

#### `DELETE /api/director/ring?addr={addr}`

Remove a peer and cancel its dial loop.

CLI: `yarilo-admin director ring remove 10.0.0.4:9102`

---

## Error responses

All errors return JSON with an `error` field and appropriate HTTP status code.

| Code | Meaning |
|:---|:---|
| `400` | Invalid request body or missing required field |
| `401` | Missing or invalid Bearer token |
| `403` | Client IP not in `allowedNets` |
| `404` | Backend not found |
| `503` | No backends available (map lookup) |

```json
{"error": "backend not found"}
```

---

## Helm values

| Value | Default | Description |
|:---|:---|:---|
| `components.director.api.port` | `9103` | API listen port |
| `components.director.api.allowedNets` | `127.0.0.0/8`, `10.96.0.0/12`, `10.244.0.0/16` | Allowed client CIDRs |
