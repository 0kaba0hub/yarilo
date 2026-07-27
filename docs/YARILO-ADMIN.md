# yarctl

Unified operator CLI for yarilo. Two top-level planes:

| Plane | Talks to | What | Subcommands |
|:---|:---|:---|:---|
| `director` | `yarilo-director` `:9103` | ring / backends / users / peers | `director status / dump / map / backends / users / ring` |
| `backend` | `yarilo-backend-api` `:9105` | per-backend storage state | `backend dict / folder / user / index / subscriptions / specialuse / metadata / who` (acl / quota land in their feature phases) |

Both planes speak JSON over plain HTTP with Bearer-token auth plus an IP
allow-list (they are in-cluster ClusterIP services, not internet-facing).
See [DIRECTOR-API.md](DIRECTOR-API.md) and [BACKEND-API.md](BACKEND-API.md)
for the wire references.

---

## Quick start

```sh
# exec into the director pod
kubectl exec -it <director-pod> -- yarctl director status

# or from outside the cluster (set URL + token explicitly)
yarctl --url http://10.0.0.1:9103 --token <token> director status
```

---

## Configuration

No flags needed when running inside the director pod.
The container already has the required environment variables set.

| Variable | Default | Description |
|:---|:---|:---|
| `YARILO_ADMIN_URL` | `http://localhost:9103` | Director API base URL (used by `director` subcommand) |
| `YARILO_ADMIN_TOKEN` | — | Director Bearer token (fallback: `DIRECTOR_API_TOKEN`) |
| `YARILO_BACKEND_API_URL` | `http://localhost:9105` | Backend API base URL (used by `backend <service>` subcommands) |
| `YARILO_BACKEND_API_TOKEN` | — | Backend API Bearer token (fallback: `BACKEND_API_TOKEN`) |

To read the auto-generated tokens from outside the pod:

```sh
kubectl get secret yarilo-director-api-token -o jsonpath='{.data.token}' | base64 -d
kubectl get secret yarilo-backend-api-token   -o jsonpath='{.data.token}' | base64 -d
```

---

## Global flags

```
yarctl [--url URL] [--token TOKEN] \
             [--backend-url URL] [--backend-token TOKEN] \
             <resource> <action> [args...]
```

| Flag | Default | Used by | Description |
|:---|:---|:---|:---|
| `--url` | `$YARILO_ADMIN_URL` or `http://localhost:9103` | `director` | Director API base URL |
| `--token` | `$YARILO_ADMIN_TOKEN` or `$DIRECTOR_API_TOKEN` | `director` | Director Bearer token |
| `--backend-url` | `$YARILO_BACKEND_API_URL` or `http://localhost:9105` | `backend <service>` | Backend API base URL |
| `--backend-token` | `$YARILO_BACKEND_API_TOKEN` or `$BACKEND_API_TOKEN` | `backend <service>` | Backend API Bearer token |

The two URLs are separate by design: director-plane ops (ring,
backends, users, peers) live on `yarilo-director:9103`; backend-plane
ops (dict / acl / quota / folder / user / mailbox) live on
`yarilo-backend-api:9105`. Each binary holds the state its plane
exposes — director state lives in director's process; backend state
(NFS + dicts) lives on backend pods.

---

## Commands

### `director status`

Ring state overview: backends and peers.

```sh
yarctl director status
```

```json
{
  "backends": [
    {"ip": "10.0.0.1", "port": 993, "tag": "ssd", "up": true, "vhosts": 100}
  ],
  "peers": ["10.0.0.2:9102"]
}
```

---

### `director dump`

Full state: backends, active user→backend entries, peers.

```sh
yarctl director dump
```

---

### `director map`

Show user→backend mappings. Without `--user` returns all active entries from userDir.
With `--user` performs a live ring lookup.

```sh
yarctl director map
yarctl director map --user alice@example.com
```

---

### `director backends list`

List all backends in the ring.

```sh
yarctl director backends list
```

---

### `director backends add`

Add a backend to the ring.

```sh
yarctl director backends add <ip> --port <port> [--tag <tag>] [--vhosts <n>]
```

```sh
yarctl director backends add 10.0.0.3 --port 993 --tag ssd
yarctl director backends add 10.0.0.4 --port 993 --tag ssd --vhosts 200
```

---

### `director backends remove`

Remove a backend from the ring.

```sh
yarctl director backends remove <ip>
```

```sh
yarctl director backends remove 10.0.0.3
```

---

### `director backends update`

Update the virtual node weight of a backend.

```sh
yarctl director backends update <ip> --vhosts <n>
```

```sh
yarctl director backends update 10.0.0.3 --vhosts 200
```

---

### `director backends up`

Mark a backend as up (resumes routing to it).

```sh
yarctl director backends up <ip>
```

---

### `director backends down`

Mark a backend as down / flush (stops new routing, existing sessions continue).

```sh
yarctl director backends down <ip>
```

---

### `director backends flush`

Flush a specific backend or all backends at once.

```sh
yarctl director backends flush <ip|all>
```

```sh
yarctl director backends flush 10.0.0.3
yarctl director backends flush all
```

---

### `director users move`

Force-assign a user to a specific backend, overriding consistent-hash routing.

```sh
yarctl director users move <user> --backend <ip:port>
```

```sh
yarctl director users move alice@example.com --backend 10.0.0.1:993
```

---

### `director users kick`

Kick a user — all active sessions for that user are terminated.

```sh
yarctl director users kick <user>
```

```sh
yarctl director users kick alice@example.com
```

---

### `director ring status`

Show the ring topology **as the queried replica sees it** (ring membership is
per-replica, so this is one replica's own view). For every member it prints the
computed left/right neighbors in `(ip,port)` order, and for this replica's
direct neighbors the live edge — role (`left`/`right`, or `both` at N=2 where a
single connection serves both directions), state (`connected`/`reconnecting`)
and uptime. It also prints each member's dedup watermark (highest `seq`
processed from that origin; `-` when none has been heard) and any tombstones
(members known dead on this replica, with the tombstone age).

```sh
yarctl director ring status
```

```
ring status: 3 directors (self 10.0.0.2:9102)
IDX  ADDR              LEFT | RIGHT                       LINK                  SEQ
0    10.0.0.1:9102     10.0.0.3:9102 | 10.0.0.2:9102      left connected 4m12s  41
1  * 10.0.0.2:9102     10.0.0.1:9102 | 10.0.0.3:9102      (self)                42
2    10.0.0.3:9102     10.0.0.2:9102 | 10.0.0.1:9102      right connected 4m12s 40
```

Use `-O json` for the structured object (`schemaVersion`, `self`, `size`,
`members[]`, `tombstones[]`) — suitable for programmatic topology assertions.

#### `--all` — cross-replica view with a health verdict

Because ring membership is per-replica, `--all` makes the queried director
aggregate every replica's own view server-side (one authorized fan-out to
peers' admin APIs) and returns a matrix plus a `healthy` verdict:

```sh
yarctl director ring status --all
```

```
ring topology: UNHEALTHY (3 replicas, 1 issue)
REPLICA           REACHABLE  SIZE  SELF-NEIGHBORS (L | R)
10.0.0.1:9102     yes        3     10.0.0.3:9102 | 10.0.0.2:9102
10.0.0.2:9102     yes        3     10.0.0.1:9102 | 10.0.0.3:9102
10.0.0.3:9102     no         -     -
issues:
  [error] peer-unreachable: 10.0.0.3:9102 is in membership but its view could not be collected
assumptions:
  - peer API endpoints derived as <ring-ip>:9103 — assumes uniform api.listen across replicas
  - admin API is plain HTTP guarded by Bearer token + api.allowed_nets; fan-out source is a director pod IP
```

The verdict flips to `UNHEALTHY` on `error`-severity issues — `peer-unreachable`
(a member whose view could not be collected — never reported as healthy),
`view-size-mismatch`, `asymmetric-edge` (A.right=B but B does not see A as its
left), and `tombstone-divergence`. `seq-lag` is `warn`-only and does **not**
fail the verdict (watermarks legitimately differ during activity). `-O json`
returns `{schemaVersion, healthy, issues[], replicas[], assumptions[]}`.

**Precondition:** the fan-out derives each peer's API endpoint from its ring IP
plus this replica's `api.listen` port, so all directors must share the same
`api.listen` (true for a Helm release sharing one ConfigMap), and `api.allowed_nets`
(if set) must include the director pod CIDR.

---

### `director ring add`

Dynamically add a peer director. Active until pod restart — for permanent peers
use `components.director.peers` in Helm values.

```sh
yarctl director ring add <addr>
```

```sh
yarctl director ring add 10.0.0.4:9102
```

---

### `director ring remove`

Disconnect a peer director.

```sh
yarctl director ring remove <addr>
```

```sh
yarctl director ring remove 10.0.0.4:9102
```

---

## Output

All commands print pretty-printed JSON to stdout. Exit code `0` on success, `1` on error.
