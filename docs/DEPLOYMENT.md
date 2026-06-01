# yarilo — deployment topology, sizing, and HA

## Architecture model

yarilo is a Go application that uses **goroutines** for concurrency, not fork-per-user as in
Dovecot C. A single process (e.g. `yarilo-imap`) serves N user sessions through goroutines
(~100 KB per session).

**Multi-binary, multi-process** (per CLAUDE.md):
- 4 separate binaries in the session role: `yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`
- 4 separate binaries in the proxy role (director): `yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-proxy`
- Each is a distinct process with its own address space
- Coordination between session processes within a backend deployment goes through `yarilo-locks`

Goroutines vs fork:
- 1000 users in a single pod ≈ 200–500 MB RAM, not 10 GB
- No "fat pod" problem
- Horizontal scaling is for HA, not for resource limits

---

## Components

### director deployment
Routes user connections to backends through a **consistent-hashing ring**.
Contains: 4 proxy processes (`yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-proxy`), 3 director processes (with monitor sidecars), peer-sync ring.
This is where **TLS terminate + passdb auth + preamble write** happens.

### backend deployment (one per tag = one per NFS shard)
Handles authenticated mail sessions, reading and writing mail + index data to NFS.
Contains: 4 session processes (`yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`) plus `yarilo-locks` for write coordination.
**Login proxies are not needed inside the backend** — it accepts plain TCP from the director with auth state in the preamble.
Userdb lookups go through the shared `yarilo-auth`.

### shared services (one deployment per installation)
- `yarilo-auth` — passdb (for the director) + userdb (for everyone)
- `yarilo-anvil` — connection/session limits (read + write from both sides)

### Why `yarilo-locks` is per backend rather than shared
- Each backend tag has its **own NFS share** — a separate data scope.
- Locks apply only to files in that share; there is no reason to coordinate with other tags.
- Lower latency (local to the backend pod).
- Blast-radius isolation: a `yarilo-locks` failure in tag A does not affect tag B.
- No global bottleneck.

### Who writes and reads `yarilo-anvil`

| Writer | What it writes |
|:---|:---|
| director's login proxies | CONNECT/DISCONNECT events (pre-auth connection tracking, per-IP rate limit) |
| backend's session processes | SESSION_START / SESSION_END (post-auth, active mail sessions) |

| Reader | When and why |
|:---|:---|
| director's login proxies | Before admitting a new connection — enforce per-user/per-IP limits on connections + sessions |

Anvil merges conn-state (from the director) with session-state (from the backend) — both written
from different places, read from a single place (the login proxy).

---

## yarilo-locks — design

**Purpose:** coordinate writes to mailbox and index files across the four session processes
(`yarilo-imap` / `yarilo-pop3` / `yarilo-submission` / `yarilo-lmtp`) — uniformly across every
deployment shape.

**Why needed:** the four binaries live in distinct address spaces. In-process `sync.Mutex` does
not cross process boundaries. In backend deployments there is the additional dimension of
coordination between StatefulSet replicas within the same tag (notably during failover).

### Deployment modes — one abstraction, two backends

A single `pkg/locks` API and a single wire protocol, with two implementations behind it.
**Production k8s always uses remote mode** (Redis-backed, mTLS TCP), regardless of how many
replicas the deployment runs — this is the only mode that supports horizontal scaling without
a config or code rework. Embedded mode is reserved for non-k8s use only.

| Mode | Production k8s (standalone or backend) | Dev / CI |
|:---|:---|:---|
| When to use | Every k8s Helm release. Scales from 1 → N replicas via `replicaCount` in values.yaml. | Local CLI runs, unit tests, single-process smoke runs. **Never in k8s.** |
| Process | `yarilo-locks` as its own Deployment (typically replicaCount=2 for HA). | `yarilo-locks --embedded` co-located in the same process tree. |
| State backend | Redis (bundled subchart or external). | In-memory map (ephemeral, pod-local). |
| Transport | mTLS TCP `:9104` reached through a ClusterIP Service. | Unix socket `/run/yarilo/locks.sock`. |
| HA | 2+ replicas behind the Service; Redis HA via Sentinel/Cluster. | None — state dies with the process. |
| Scales to N session pods | Yes. All session pods reach the same locks Service and share state via Redis. | No. Unix socket is pod-local and in-memory state is per-process. |
| RTT (typical) | ~1–2 ms (in-cluster Redis on the same node ~0.5 ms). | ~100–300 µs. |
| Wire protocol | identical | identical |

**Why embedded is not a k8s option, even at `replicaCount=1`:**
- Unix-socket coordination is pod-local. Bump `replicaCount: 1 → 2` and the second pod
  cannot see lock state held by the first — two writers collide on the same NFS file.
- A scheduled rolling restart of a single replica drops every in-flight lock for ~30 s.
  Remote mode survives this: clients reconnect to the surviving replica and pick up state
  from Redis.
- Operator surprise is the worst kind of incident. The deployment must not silently switch
  semantics when the operator scales it up. One mode in production, end of story.

**Two-tier locking convention:** `sync.Mutex` inside a process is the fast-path for
goroutine-level contention (no RTT). `yarilo-locks` (always remote, in production) is engaged
only when a cross-process barrier is required (any write to a shared file).

### Configuration

Two independent sections in `yarilo.yaml`. The yarilo-locks process consumes
`locks_service`; every session binary (yarilo-imap, yarilo-pop3, yarilo-submission,
yarilo-lmtp) consumes `locks_client`. mTLS material is shared with the rest of
the stack via `internal_tls` — no separate keys live under the locks sections.

```yaml
# k8s production (standalone or backend): yarilo-locks listens on TCP+mTLS,
# Redis backs the state.
locks_service:
  mode: remote
  listen: ":9104"
  redis: "redis://redis.yarilo.svc.cluster.local:6379/0"

# session binaries reach yarilo-locks via the ClusterIP Service.
locks_client:
  mode: remote
  endpoints: ["yarilo-locks.yarilo.svc.cluster.local:9104"]

internal_tls:
  enabled: true
  cert: /etc/yarilo/tls/tls.crt
  key:  /etc/yarilo/tls/tls.key
  ca:   /etc/yarilo/tls/ca.crt
```

```yaml
# dev / unit tests / non-k8s CLI runs (single process, no Redis).
locks_service:
  mode: embedded
  socket: /run/yarilo/locks.sock

locks_client:
  mode: embedded
  socket: /run/yarilo/locks.sock
```

### Lock model

- **Exclusive (X) locks only** for writes.
- **Reads take no lock** — the storage layer provides a consistent snapshot.
- TTL-based (auto-release on client crash, typically 30 s with renew every 10 s).
- Granularity: per-mailbox (`mbox:<user>:<folder>`).

| Operation | Lock |
|:---|:---|
| IMAP SELECT / FETCH / SEARCH / IDLE | none |
| LIST / LSUB | none |
| IMAP APPEND / EXPUNGE / STORE | X on mailbox |
| LMTP delivery | X on mailbox |
| Rename / Delete mailbox | X on mailbox |
| Sieve script update | X on user-scripts |

### Wire protocol — identical across both modes

TAB-delimited, LF-terminated. In remote mode the transport is TCP over mTLS on port `:9104`.
In embedded mode the identical byte stream runs over the Unix socket `/run/yarilo/locks.sock`
(no TLS).

```
> VERSION\t1\n
< VERSION\t1\tOK\n

> LOCK\t<resource>\t<owner>\t<ttl_ms>\n
< OK\t<lock_id>\n           # acquired
< BUSY\t<current_owner>\n   # held by someone else

> UNLOCK\t<lock_id>\n
< OK\n | NOT_FOUND\n

> RENEW\t<lock_id>\t<new_ttl>\n
< OK\n | EXPIRED\n

> EVENT\t<resource>\t<event_type>\t<payload>\n  # optional emit for external consumers
```

### What stays inside the session processes

- Writing raw mail data (dbox segments, maildir files).
- Writing index updates (under the X lock on the mailbox).
- UID assignment (under the X lock — atomic read-increment-write of NEXTUID).
- IDLE notifications published over the `yarilo-locks` EVENT channel.

### Deadlock prevention

Code convention: always acquire locks in the order
`idx:<user>` → `mbox:<user>:<folder>` → `deliver:<user>:<folder>`.
If anything hangs, the TTL releases the lock automatically.

### Storage backend (remote mode only)

Redis (per backend deployment, or in the same namespace). Key: `lock:<resource>`,
Value: `<owner>|<acquired_at>`, TTL: 30 s. Atomic acquisition via Lua `SET ... NX EX`.

Embedded mode keeps the same key/value shape in a local `map[string]lockState` with a
background TTL sweeper — no external dependencies.

### HA (remote mode only)

- 2 replicas of `yarilo-locks` per backend deployment, behind a ClusterIP Service.
- Stateless (state in Redis).
- Local Redis, or shared with other components (anvil, etc.).

Embedded mode has no HA — state is ephemeral; on process crash every lock is lost. Acceptable
for unit tests and CLI dev runs; not used in k8s deployments.

---

## Standalone deployment — single-node k8s, scale-out by replicaCount

The standalone deployment targets a single k8s node, no director, no per-tag sharding. It is
**built to scale from 1 → N replicas of each component by editing values.yaml** — no rewiring,
no protocol changes, no code touched. The same pattern carries the operator from a one-pod dev
cluster to a small multi-replica production setup.

### Components (all as k8s Deployments unless noted)

| Component | Default replicas | Scale by |
|:---|:---|:---|
| `yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login` | 1 each | `replicaCount` per protocol (login is stateless beyond TLS state) |
| `yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp` | 1 each | `replicaCount` per protocol (coordination via locks); `yarilo-lmtp` is MTA-facing and has no login proxy — its Service is reached directly by upstream MTAs |
| `yarilo-auth` | 1 | `replicaCount` (stateless; userdb in SQL) |
| `yarilo-anvil` | 1 | `replicaCount` (state in Redis) |
| `yarilo-locks` | 2 | `replicaCount` (state in Redis; 2 = HA default) |
| `redis` | 1 (StatefulSet) | external HA or Sentinel for production |

### Storage

A single `PersistentVolumeClaim` with `accessModes: [ReadWriteMany]` — every session pod mounts
it at the same path. On single-node clusters this can be backed by `hostPath`, NFS, or a CSI
RWX provisioner; on multi-node clusters it must be NFS or CephFS.

### Routing

A k8s `Service` per public port (993, 995, 465, 587, 143, 110) load-balances connections
across the matching login pods. Port `24` is a `Service` in front of `yarilo-lmtp` directly —
LMTP is MTA-facing and authenticates via the SMTP envelope, so no separate login proxy.
There is **no director** — sessions distribute round-robin (or
by k8s `Service`'s sessionAffinity setting). Cross-pod write contention is resolved through
`yarilo-locks`; that adds RTT but stays correct. Once cross-pod contention is a measured
problem, the upgrade path is a director deployment (separate document); the session and login
binaries do not change.

### Scale invariants

- **`yarilo-locks` is always remote mode**, even at `replicaCount=1` for every other component.
  This is what keeps the deployment scalable without rework.
- **All session processes call the locks Service via mTLS TCP.** Storage code uses
  `pkg/locks.Locker` only; no compile-time switch on deployment shape.
- **Storage is RWX from day one.** Switching from RWO to RWX later requires a data migration
  and a full re-deploy. Doing it up front costs nothing extra.

### Out of scope for standalone

- Per-tag sharding (multiple NFS shares, different user populations on different backends).
  That belongs to the backend deployment (see `docs/yarilo_backend.svg`).
- Director-based sticky routing and consistent hashing. Standalone uses k8s `Service`
  load-balancing; if measured contention warrants it, switch to director without touching
  the session binaries.

---

## Helm chart structure

```
helm/yarilo-shared        → auth + anvil + Redis (shared across the installation)
helm/yarilo-director      → director pool + monitor sidecars
helm/yarilo-backend       → backend pool (one release per tag = per NFS shard, with its own locks)
```

### yarilo-shared
- `Deployment yarilo-auth` — replicaCount=2, stateless (userdb in an external SQL/LDAP).
- `Deployment yarilo-anvil` — replicaCount=2, state in Redis.
- `Deployment redis` (or external) — state backend for anvil.
- A ClusterIP Service for each.

### yarilo-director
- `StatefulSet yarilo-director` — replicaCount=3 (peer-sync ring).
- 2 containers per pod: `yarilo-director` + `yarilo-monitor` (sidecar).
- 4 login-proxy processes (`yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-proxy`) — in separate containers or under a master-supervised process tree.
- ClusterIP Service — public entry point: :993/:995/:587/:24.
- Headless Service — for peer-sync DNS.

### yarilo-backend (one release per tag, e.g. `yarilo-backend-a`)
**4 separate StatefulSets (one per protocol)** within a single Helm release, for independent scaling:

- `StatefulSet yarilo-backend-<tag>-imap` — replicaCount=N (HPA: connection count).
- `StatefulSet yarilo-backend-<tag>-pop3` — replicaCount=M (HPA: poll rate, typically small).
- `StatefulSet yarilo-backend-<tag>-submission` — replicaCount=P (HPA: outbound rate).
- `StatefulSet yarilo-backend-<tag>-lmtp` — replicaCount=Q (HPA: delivery queue, scaled on burst).
- `Deployment yarilo-locks-<tag>` — replicaCount=2, cross-protocol coordination.
- `Deployment redis-<tag>` (or shared Redis) — state backend for locks.
- One **PVC NFS (RWX)** — shared by all 4 StatefulSets within the tag.
- 4 Headless Services — one per StatefulSet, for sticky routing from the director.

**Why 4 separate StatefulSets and not 1 with 4 containers in a pod:**
- Process isolation — an `imap` crash does not affect `lmtp`.
- Independent scaling — POP3 typically runs as 1 pod, lmtp at 10+ during mass delivery.
- Right-sized resources — each StatefulSet has its own CPU/RAM limits.
- HPA per protocol driven by different metrics.

**Trade-off:** Cross-protocol writes (e.g. LMTP delivery → IMAP STORE on the same mailbox) go
through `yarilo-locks` (cross-pod). Locks becomes the **critical path** for every write.

### Director routing — 4 separate rings

The director maintains 4 independent rings (one per protocol):
- `ring_imap`: `MD5(user) → yarilo-backend-<tag>-imap-N`
- `ring_pop3`: `MD5(user) → yarilo-backend-<tag>-pop3-M`
- `ring_submission`: ...
- `ring_lmtp`: ...

N, M, P, Q can differ (different StatefulSet sizes). For a single user, IMAP and LMTP may land
on different pods — coordination goes through `yarilo-locks`.

---

## Sizing per backend pod

| Workload | RAM | CPU |
|:---|:---|:---|
| 5k idle (mostly IMAP IDLE) | ~300–500 MB | ~0.1 cores |
| 2k active | ~500–800 MB | ~0.3–0.5 cores |
| Burst (FETCH of a large mail × 100 users) | up to ~2 GB peak | up to 1–2 cores peak |

**Recommended Helm values:**
```yaml
resources:
  requests:
    cpu: 250m
    memory: 512Mi
  limits:
    cpu: 2000m
    memory: 2Gi
```

**Bottlenecks (in order):**
1. **Open FDs** — `ulimit -n 65535` is required (default 1024 is too low). ~10 FD per session.
2. **NFS IOPS** — the most likely bottleneck. SSD-backed NFS plus `cachefilesd`/`fscache` help.
3. **yarilo-locks RTT** — every write does LOCK/UNLOCK. Local Redis → ~1–2 ms per pair.
4. **NFS bandwidth** — attachment FETCH. 1 Gbps = 125 MB/s.
5. **RAM on burst** — buffers for reading large mails.

---

## Sizing per backend tag (4 StatefulSets + one NFS share + local locks)

| Parameter | Typical | Min/Max |
|:---|:---|:---|
| yarilo-imap replicaCount | 3–5 | 1 / scale-out |
| yarilo-pop3 replicaCount | 1–2 | 1 / scale-out (POP3 is rarely intensive) |
| yarilo-submission replicaCount | 2–3 | 1 / scale-out |
| yarilo-lmtp replicaCount | 3–5 baseline | 1 / 10+ during mass-delivery burst |
| locks replicaCount | **2** | stateless, ClusterIP |
| Users per pod | **3–5k** mostly idle | Goroutines are cheap, but FD/RAM limits apply |
| **Total users per tag** | **10–20k** | NFS server is the constraint |
| NFS share size | **5–10 TB** | 10k users × ~500 MB average quota |
| NFS sustained IOPS | **10–30k** | Index/mailbox operations |
| NFS bandwidth | **1–10 Gbps** | Attachment transfer |

---

## When to create a new tag

| Trigger | Threshold |
|:---|:---|
| NFS storage used | > 70–75% |
| NFS sustained IOPS | > 70% of capacity |
| Users per tag | > 15–20k |
| p99 latency for simple IMAP ops | > 200 ms |

One Helm release `yarilo-backend` per tag: `yarilo-backend-a`, `yarilo-backend-b`, …
Each with its own NFS PV and its own `yarilo-locks` service.

**Scale examples:**
- 10k users → 1 tag, 3 replicas, 5 TB NFS.
- 50k users → 3–4 tags, each with 3 replicas × 5 TB.
- 200k+ → 10–15 tags; start considering non-NFS storage.

---

## Director routing & stickiness

**Ring:** `MD5(username) → backend pod` (within a single tag).
**Tag assignment:** a separate user → tag map (admin-defined or hash-based shard).

1. Client → director's login proxy (TLS terminate).
2. Director: passdb via `yarilo-auth`, userdb as well.
3. Director: determines the user's tag; the ring maps user → pod.
4. Director connects directly to the pod via stable DNS (headless Service).
5. Passes auth state in the preamble, proxies plain TCP.

`userDir` in the director is an in-memory cache of active user → pod mappings.
Synced between directors over the peer protocol.

---

## HA strategy

| Layer | HA approach |
|:---|:---|
| Director | replicaCount=3, peer-sync, monitor sidecar |
| Backend per tag | replicaCount=3–5, shared NFS RWX, ring rebalance |
| yarilo-locks (per tag) | replicaCount=2, state in Redis |
| yarilo-auth | replicaCount=2, stateless |
| yarilo-anvil | replicaCount=2, state in Redis |
| Redis | external HA (Sentinel/Cluster) or managed |
| NFS server | a separate HA effort (Pacemaker+DRBD, or managed NFS such as AWS EFS) |

**Backend failover sequence:**
1. The `yarilo-monitor` sidecar detects a dead pod (~5–10 s).
2. Director removes it from the ring.
3. Locks on the dead pod expire via TTL (30 s) on `yarilo-locks-<tag>`.
4. Ring rehash → users move to neighbouring replicas in the same tag.
5. The k8s scheduler brings up a new pod (~30 s).
6. The new pod mounts the same NFS and starts accepting connections.

---

## Stickiness rationale

User X is always served by a single backend pod (within the same tag). Reasons:
- Less cross-pod lock contention in `yarilo-locks`.
- Index cache locality.
- Avoid duplicated IDLE notifications.
- Faster session startup (cache prewarmed).

Stickiness ≠ data partitioning. Data is shared on NFS; sticky routing is an optimisation.

---

## Why event-loop (goroutines), not fork

yarilo is written in Go. Go runtime + `fork()` = undefined behaviour — forbidden by CLAUDE.md.
- `exec.Command` — to launch child processes at pod startup.
- **Goroutines per user** within a process — one goroutine per user session.

This is fundamentally different from the Dovecot C model (fork per user → process per user →
~10 MB per session). yarilo holds 1000+ users in a single process without resource overhead.
