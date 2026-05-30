# yarilo-admin

Unified operator CLI for yarilo. Every subsystem that needs an "ops
surface" gains a subcommand here:

| Subsystem | Purpose | Doc |
|:---|:---|:---|
| `director` | Manage `yarilo-director` cluster via HTTP admin API | this page |
| `dict` | Operate on `pkg/dict` KV stores (metadata, quota, ACL state, ...) | [DICT.md](DICT.md) |

Runs inside the director pod for `director` ops — no flags required
in standard k8s deployments. Runs anywhere with config / driver access
for `dict` ops.

---

## Quick start

```sh
# exec into the director pod
kubectl exec -it <director-pod> -- yarilo-admin director status

# or from outside the cluster (set URL + token explicitly)
yarilo-admin --url http://10.0.0.1:9103 --token <token> director status
```

---

## Configuration

No flags needed when running inside the director pod.
The container already has the required environment variables set.

| Variable | Default | Description |
|:---|:---|:---|
| `YARILO_ADMIN_URL` | `http://localhost:9103` | API base URL |
| `YARILO_ADMIN_TOKEN` | — | Bearer token (fallback: `DIRECTOR_API_TOKEN`) |

To read the auto-generated token from outside the pod:

```sh
kubectl get secret yarilo-director-api-token -o jsonpath='{.data.token}' | base64 -d
```

---

## Global flags

```
yarilo-admin [--url URL] [--token TOKEN] <resource> <action> [args...]
```

| Flag | Default | Description |
|:---|:---|:---|
| `--url` | `$YARILO_ADMIN_URL` or `http://localhost:9103` | API base URL |
| `--token` | `$YARILO_ADMIN_TOKEN` or `$DIRECTOR_API_TOKEN` | Bearer token |

---

## Commands

### `director status`

Ring state overview: backends and peers.

```sh
yarilo-admin director status
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
yarilo-admin director dump
```

---

### `director map`

Show user→backend mappings. Without `--user` returns all active entries from userDir.
With `--user` performs a live ring lookup.

```sh
yarilo-admin director map
yarilo-admin director map --user alice@example.com
```

---

### `director backends list`

List all backends in the ring.

```sh
yarilo-admin director backends list
```

---

### `director backends add`

Add a backend to the ring.

```sh
yarilo-admin director backends add <ip> --port <port> [--tag <tag>] [--vhosts <n>]
```

```sh
yarilo-admin director backends add 10.0.0.3 --port 993 --tag ssd
yarilo-admin director backends add 10.0.0.4 --port 993 --tag ssd --vhosts 200
```

---

### `director backends remove`

Remove a backend from the ring.

```sh
yarilo-admin director backends remove <ip>
```

```sh
yarilo-admin director backends remove 10.0.0.3
```

---

### `director backends update`

Update the virtual node weight of a backend.

```sh
yarilo-admin director backends update <ip> --vhosts <n>
```

```sh
yarilo-admin director backends update 10.0.0.3 --vhosts 200
```

---

### `director backends up`

Mark a backend as up (resumes routing to it).

```sh
yarilo-admin director backends up <ip>
```

---

### `director backends down`

Mark a backend as down / flush (stops new routing, existing sessions continue).

```sh
yarilo-admin director backends down <ip>
```

---

### `director backends flush`

Flush a specific backend or all backends at once.

```sh
yarilo-admin director backends flush <ip|all>
```

```sh
yarilo-admin director backends flush 10.0.0.3
yarilo-admin director backends flush all
```

---

### `director users move`

Force-assign a user to a specific backend, overriding consistent-hash routing.

```sh
yarilo-admin director users move <user> --backend <ip:port>
```

```sh
yarilo-admin director users move alice@example.com --backend 10.0.0.1:993
```

---

### `director users kick`

Kick a user — all active sessions for that user are terminated.

```sh
yarilo-admin director users kick <user>
```

```sh
yarilo-admin director users kick alice@example.com
```

---

### `director ring status`

List all currently connected director peers.

```sh
yarilo-admin director ring status
```

---

### `director ring add`

Dynamically add a peer director. Active until pod restart — for permanent peers
use `components.director.peers` in Helm values.

```sh
yarilo-admin director ring add <addr>
```

```sh
yarilo-admin director ring add 10.0.0.4:9102
```

---

### `director ring remove`

Disconnect a peer director.

```sh
yarilo-admin director ring remove <addr>
```

```sh
yarilo-admin director ring remove 10.0.0.4:9102
```

---

## Output

All commands print pretty-printed JSON to stdout. Exit code `0` on success, `1` on error.
