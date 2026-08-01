# yarilo — deployment topology, sizing, and HA

## Architecture model

yarilo is a Go application that uses **goroutines** for concurrency, not fork-per-user as in
The reference C. A single process (e.g. `yarilo-imap`) serves N user sessions through goroutines
(~100 KB per session).

**Multi-binary, multi-process** (per CLAUDE.md):
- 4 separate binaries in the session role: `yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`
- 4 separate binaries in the proxy role (director): `yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-login`
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
Contains: 4 proxy processes (`yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-login`), 3 director processes, self-organizing ring (#750).
This is where **TLS terminate + passdb auth + allow_nets enforcement** happens.

**Login pod auth flow:**
1. Accept TLS connection from client.
2. Extract real client IP from HAProxy PROXY protocol header or raw TCP RemoteAddr.
3. Parse the IMAP/POP3/SMTP AUTHENTICATE or LOGIN command to obtain credentials.
4. Call **yarilo-auth** (`AUTH` command) — passdb chain runs there; receives `token=` on success.
   `allow_nets` and `nologin` are enforced in the login pod using the real client IP.
5. On failure: send `NO Invalid credentials` to the client. Backend is never contacted.
6. On success: send proto-specific auth OK to the client, then dial the backend and send:
   ```
   YARILO\tADDR=<real-client-ip>\tSESSION=<anvil-id>\tUSER=<username>\tTOKEN=<64hex>\n
   ```
7. Backend reads the preamble, calls yarilo-auth `VERIFY token` to confirm the session,
   and enters pre-authenticated state. No passdb round-trip on the backend side.

### backend deployment (one per tag = one per NFS shard)
Handles authenticated mail sessions, reading and writing mail + index data to NFS.

**Co-located pod (one pod serves ALL of a user's per-user state).** A backend pod
is a **single StatefulSet** whose pod runs one container per protocol —
`yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`,
`yarilo-managesieve` — plus the `yarilo-fts` full-text-search container, the
`yarilo-backend-api` admin container, and a `yarilo-backend-reg` registration
sidecar, sharing the pod's **one IP** and the tag's **one NFS PV (RWX)**.
`yarilo-locks` runs as its own per-tag Deployment for cross-pod write
coordination.

**What goes in the pod vs stays separate — the criterion.** The pod holds
everything that owns **per-user write state**: the protocol containers, `fts`
(per-user Xapian index), and `backend-api` (its `fts rescan`/`optimize` and
direct mailbox/index access write a user's data — as a standalone Deployment it
would be a *second* writer of the same index/mailbox the user's sticky pod owns,
the #675/#676 hazard; in-pod it uses that pod's localhost fts, so single-writer
holds). `backend-api` listens on the **pod IP only**; `yarctl` reaches the
owning pod by doing a director LOOKUP itself and dialing that pod (#792).
**Global-read services with stable external consumers stay separate** —
`yarilo-quota-status` is a Postfix policy service the external MTA dials at a
fixed endpoint, and it only reads a shared quota dict (any instance answers any
user, no per-user write), so co-locating it would break the MTA contract for
zero gain. It stays its own Deployment.

This is deliberate and load-bearing for the whole director model: it restores the
The reference invariant **one mail-host owns every per-user resource**. Because the pod
at a given IP answers imap *and* pop3 *and* lmtp *and* submission *and*
managesieve, the director needs only **one ring and one user→pod map** — a user
hashes to one pod IP, and that IP is correct for every protocol (the login proxy
dials the protocol-specific port on that same IP). See "Director routing — one
ring" and "Why co-located, not per-protocol StatefulSets" below.

**FTS is co-located for the same reason — and it closes #675/#676.** The Xapian
index is **per-user**, and a session reaches FTS over `ftsproto`. Today
`fts.fts_addr` is a shared ClusterIP, so requests for one user's index
round-robin across `yarilo-fts` replicas → two pods cache the same user's write
handle → corruption (why `replicas > 1` is a footgun, #676; the write-handle
hand-off #675). Co-locating `yarilo-fts` in the pod with `fts_addr = localhost`
makes the user's **already-sticky** pod the sole owner of that user's index write
handle. On the shared NFS every per-user Xapian dir then has exactly one writer —
the same "1 user = 1 pod" invariant that fixes #788 gives FTS single-writer for
free, so `replicas > N` is safe (indexing load follows the users on each pod).

**Login proxies are not needed inside the backend** — it accepts plain TCP from the director
with auth state in the YARILO preamble. The login pods (`yarilo-imap-login`, …,
`yarilo-sasl-login`) live in the **director deployment** (see
`docs/yarilo_director.svg`), never here.

Backend auth logic:
- Login pod sends `YARILO\tADDR=...\tSESSION=...\tUSER=...\tTOKEN=...\n` before any protocol exchange.
- Backend's `PreambleListener` reads the preamble, calls yarilo-auth VERIFY (`service=` field enforced), enters pre-authenticated state.
- All 4 backends accept only the YARILO preamble from login pods (director is control-plane only — it never touches client bytes; #741). Client-IP forwarding into the *login* layer, when needed, is haproxy protocol / native inbound (ID/XCLIENT, #742) / none, chosen per protocol — it never reaches the backend directly. LMTP preamble originates from `yarilo-lmtp-login`.

### backend liveness — self-registration + heartbeat (#776)

Backends are registered in the director ring by **self-registration + heartbeat**,
not by a one-time DNS resolve or an external prober. This makes the backend list a
second application of the exact lifecycle machinery already built for the director
ring itself (JOIN on start, ring-wide keepalive, graceful leave #770, Lamport
ordering #772, tombstone-free convergence) rather than a separate DNS-poll
mechanism — and it works in **every** topology (k8s and non-k8s standalone),
consistent with config-not-binary. Supersedes the DNS-reconcile approach (closed
PR #778).

**One registration per POD, not per protocol.** Because the pod is co-located,
exactly **one** `yarilo-backend-reg` sidecar per pod owns the director
registration — it registers the **pod IP once**, not one stream per protocol
container. This is not cosmetic: four protocol containers each registering the
same IP would push four independent monotonic-seq streams for one origin, and the
strictly-newer lease dedup (#776) compares seq only within an origin — four
interleaved streams on one IP would fight, each rejecting the others as stale. One
sidecar = one origin = one clean seq stream. The sidecar reuses the exact
`internal/backendreg` client (including the #787 fixes: PONG on the director's
PING, and reconnect-backoff reset on a healthy long-lived connection — without
those a co-located pod would flap **as a whole**, a larger blast radius than the
per-protocol flap #787 first surfaced).

Model:
- **Register on start.** The `yarilo-backend-reg` sidecar dials the director
  **ClusterIP Service** — it does not matter which director replica answers; a
  load-balanced address is correct here precisely because the registration is
  gossiped **ring-wide**. It sends `BACKEND-UP\t{ip}\t{port}\t{tag}\t{vhosts}\t{seq}`
  for the **pod IP**; the receiving director adds it and forwards it as a
  `RING-CHANGE up` envelope (carrying port + vhosts, #776) so every director learns
  it. `{port}` is nominal — login proxies override it with their own protocol port
  (`internal/login` BackendPort / lmtp-login BackendPort), dialing
  `{pod-ip}:{protocol-port}`; the ring entry identifies the POD, the login picks the
  protocol port. (This is the only backend→director control channel — otherwise the
  director only ever dials backends.)
- **`{tag}` is the STORAGE SHARD, never the protocol.** A co-located pod serves all
  protocols, so protocol is not a routing dimension — the tag stays a pure NFS-shard
  label (one tag = one NFS PV = one `yarilo-backend` release). Per-user shard
  selection (userdb `director_tag`, #746) is unaffected.
- **Heartbeat = periodic re-register, gated on the PROTOCOL containers'
  self-health.** Every `backend_register_interval` the sidecar re-sends
  `BACKEND-UP` — but ONLY while **every protocol** container in the pod is serving
  (each protocol's `/readyz`: listener accepting and not wedged). A heartbeat must
  prove the DATA PATH is alive, not merely that a control goroutine still ticks.
  **Readiness-gate semantics (spell this out for operators):** because it is one
  pod = one ring entry, a *single* unready protocol container makes the whole pod
  go silent → the lease expires → the pod is removed for **all** protocols. This is
  correct and matches the reference (host down = down for everything on that host), but it
  is a real coupling: an NFS-wedged `yarilo-lmtp` will pull the pod's IMAP traffic
  off too. That is the price of the one-mail-host invariant, and the sidecar's gate
  makes the wedge fail safe (silence → expiry) rather than routing to a half-dead
  pod. **`yarilo-fts` is deliberately NOT in the gate:** FTS is best-effort (a
  wedged index degrades SEARCH to a slow fallback but never blocks mail flow), so
  an unhealthy fts container must not evict the pod's live IMAP/LMTP traffic. Each heartbeat
  carries a **monotonic per-origin sequence** (a logical counter, NOT wall-clock —
  the Lamport lesson of #772: pod clocks are unsynchronized, so freshness is
  compared only *within* one backend's own origin; the seq is seeded from the
  process start time so a same-IP restart resumes above the director's last-recorded
  seq, #776), gossiped ring-wide as "backend X seen at seq N". Every director keeps
  the max seen per backend; a heartbeat landing on **any** director refreshes the
  lease everywhere — this is what makes a load-balanced heartbeat correct (a
  per-director timer with LB'd heartbeats would false-expire a live backend on the
  directors that happened not to receive it).
- **TTL expiry → LEAVE (remove + rehash).** A backend whose last-seen has not
  advanced within `backend_expire` on the ring is REMOVED (`RING-CHANGE down` →
  `RemoveBackend` everywhere); its hash-slice rehashes onto neighbouring backends
  in the same tag. This covers the silent hang / crash with no external prober.
- **Graceful leave → LEAVE (remove + rehash).** On SIGTERM the backend sends
  `BACKEND-DOWN` (analog of the director's #770 graceful leave) so it is removed
  immediately without waiting out the TTL — a planned rollout / scale-down never
  blackholes a hash-slice for the expiry window.
- **Overload → FLUSH (drain, NO rehash), never DOWN.** Removal and overload are
  DIFFERENT wire events with different consequences and must not be conflated. A
  backend shedding load sends `BACKEND-FLUSH`: it STAYS in the ring, new LOOKUPs
  stop landing on it, existing sessions keep running — its hash-slice does NOT
  move. A transient load spike therefore causes zero user reshuffling. Sending
  `BACKEND-DOWN` on overload would remove it → rehash the slice away → and then a
  second rehash back when it re-registers a minute later: double-shuffling users
  through a transient peak, worse than the peak itself. This matches the reference's
  down/vhosts semantics (drain by zeroing vhosts, not by removal). `BACKEND-DOWN`
  is reserved for genuine LEAVE (SIGTERM / expiry).

Safety + migration:
- **Never remove the last backend of a tag** by TTL expiry — a suspect-but-only
  backend is kept (and logged loudly) rather than guaranteeing a total blackhole
  for that tag.
- **Static `mail_servers` entries stay non-expiring** — they are a bootstrap /
  non-k8s fallback and are never pruned by the heartbeat lease, so registration can
  be rolled out incrementally with no binary branching (config-not-binary).
- **admin-API `backends add/remove` stay manual and non-expiring** — an operator's
  explicit entry is never touched by the lease.
- **No tombstone needed** (unlike ring members whose ephemeral pod identity never
  returns): a dead backend heartbeats nowhere and never re-registers, so a gossiped
  ghost simply ages out at TTL.

Config (snake_case, section-prefixed; `yarilo.yaml` + Helm `values.yaml`):
- `director_service.backend_expire` — seconds of missed heartbeat before a backend
  is removed ring-wide.
- sidecar side: `backend_register.director_addr` (director ClusterIP),
  `backend_register.register_interval`, `backend_register.tag` (NFS shard),
  `backend_register.vhosts`. One `backend_register` block per pod drives the single
  `yarilo-backend-reg` sidecar; there is **no** per-protocol registration path
  (config-not-binary: one registration mechanism, no modes).

### backend process liveness — the watchdog probe (#904)

Distinct from ring registration above: that governs whether a backend is a
routing target, this governs whether a wedged container is **restarted**.

The three probes on each co-located protocol container answer three different
questions, and only one of them can restart the container:

- **startupProbe** — dependencies (locks, Redis, DB) are reachable before the
  container takes traffic (#903). Runs once at start.
- **readinessProbe** — the pod's per-protocol freshness file is fresh (#788), so
  the login proxy only routes to a container that is currently serving. Removes
  from endpoints, never restarts.
- **livenessProbe** — `/healthz`, which is unconditional until the **liveness
  watchdog** trips it. The watchdog is a timer-driven self-check (stat the mail
  store base, enter a local gate) that fails `/healthz` when the request path is
  wedged in a way nothing else sees: a stale NFS handle hanging every mailbox op,
  or an in-process deadlock, while the accept loop and this HTTP server keep
  answering. Restarting is then the only correct response, and liveness is the
  only probe that restarts.

**Why liveness is gated on the watchdog.** Without a self-check, `/healthz`
answers 200 as long as the telemetry server runs, so a liveness probe on it is
inert — it can only ever catch the telemetry server itself dying, which the
process's own `os.Exit(1)` on a dead accept loop already covers. So the backend
carried no livenessProbe historically. The probe is therefore rendered **only
when `telemetry.livenessWatchdog.enabled`** is set: enabling the watchdog is what
makes `/healthz` a real signal, and only then does watching it with a restart
make sense. Off by default preserves the prior no-restart behaviour exactly.

**Guard rails against restarting a healthy busy pod:** the watchdog trips only
after several consecutive failed self-checks, the self-check timeout is shorter
than its interval, the probe has its own `failureThreshold` on top, and the
self-check touches **nothing shared** — so a hung NFS handle restarts the one pod
that mounts it, and a database hiccup restarts nothing. A single protocol
container restarts alone; the other protocols on the pod keep serving.

Config (snake_case, section-prefixed; `yarilo.yaml` + Helm `values.yaml`):
`telemetry.liveness_watchdog.liveness_watchdog_enabled` / `_interval_seconds` /
`_timeout_seconds` / `_failure_threshold`, and `_fault_injection_enabled` — a
self-destruct switch that exposes `POST /debug/fault/deadlock` to confirm the
restart path on a live pod, off by default.

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
| director's login proxies (imap/pop3/submission) | CONNECT/DISCONNECT events (pre-auth connection tracking, per-IP rate limit) |
| `yarilo-lmtp-login` | CONNECT/DISCONNECT per recipient (per-user delivery rate tracking) |
| backend's session processes (imap/pop3/submission) | SESSION_START / SESSION_END (post-auth, active mail sessions) |

| Reader | When and why |
|:---|:---|
| director's login proxies | Before admitting a new connection — enforce per-user/per-IP limits on connections + sessions |
| `yarilo-lmtp-login` | Per RCPT TO — enforce per-user delivery concurrency limits before fan-out |

Anvil merges conn-state (from the director / lmtp-login) with session-state (from the backends) — written
from different processes, read from a single place (the login proxy).

### Scaling `yarilo-anvil` — 1 → N replicas (#908)

Anvil holds three pieces of shared state: the per-IP auth-failure **penalty** counter, the
per-`user@ip` **session** accounting (the connection limit), and the **kick bus** (`EMIT`/`SUBSCRIBE`).
Where that state lives is a single config choice — `components.anvil.service.state_backend`:

| `state_backend` | State lives in | Replicas | Deploy strategy |
|:---|:---|:---|:---|
| `memory` (default) | the pod's process | **must be 1** | `Recreate` |
| `redis` | shared Redis | any N | `RollingUpdate` |

- **`memory`** is the standalone / dev / test default. All three pieces of state are per-pod, so a
  second replica would see a *different* subset of sessions, enforce the limit independently, and — the
  decisive one — **miss kicks**: a login pod's `SUBSCRIBE` lands (via the ClusterIP) on one pod while an
  `EMIT` may land on another, and an in-process bus never crosses that gap. The chart therefore **fails
  closed** on `replicas > 1` with `memory`, and the deploy strategy is `Recreate` so a rolling update
  never briefly runs two memory pods and splits their state.
- **`redis`** moves all three pieces into shared Redis: penalty is a key with TTL, sessions are hashes +
  an atomic counter reconciled by a sweep, and the kick bus is Redis **Pub/Sub** — an `EMIT` on any pod
  `PUBLISH`es, and every pod's subscribers receive it, so a kick published on pod-B is delivered by
  pod-A. Pods are then stateless and scale to any N behind the existing ClusterIP; the strategy is a
  normal `RollingUpdate`. This is the required setting before raising `replicas`.

Both the subscribe path (go-redis auto-reconnects and re-subscribes on a Redis blip) and the
login→anvil transport (the login pod redials + re-subscribes) recover on their own, so a Redis or anvil
restart does not permanently deafen a pod to kicks. Kick delivery is best-effort / at-most-once and is
**not** a correctness guarantee — the director's confirmed ring-wide kill (#847) holds `LOOKUP` until the
ring-wide session count is stably zero, so split-writer safety never depends on a kick landing; the kick
only makes teardown prompt.

Observability for the scale-out (all always-on, no debug flag): `yarilo_anvil_connect_total{result}`,
`yarilo_anvil_kick_emitted_total` / `yarilo_anvil_kick_delivered_total` (different pods, proving
cross-replica delivery under a scrape), `yarilo_anvil_redis_errors_total{op}` (fail-open made visible),
`yarilo_anvil_reconcile_adjustments_total`, and `yarilo_anvil_penalty_updates_total{result}`. Structured
logs carry `pod=` so a kick can be traced across replicas directly.

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

#### Internal client dials — `internal_tls.server_name` (#816)

Every internal **client** dial (auth / anvil / locks / backend-api / backend /
fts / the login pods) verifies the peer against a single pinned name,
`internal_tls.server_name` — **not** the dialed host. Internal services are
reached by short name, FQDN, or pod IP interchangeably, so host-based
verification is unreliable; the shared internal-tls cert instead carries one
stable SAN and every client pins it. Because all internal components share one
cert, mutual auth attests **"cluster member"**, not a specific service identity
(true per-service identity would need per-service certs — a separate redesign).

The chart defaults `server_name` to `<release>-internal` and injects that name
into the director cert SANs; **the shared internal-tls secret you provide MUST
carry `<release>-internal` as a SAN** (`hack/internal-ca-sandbox.yaml` shows a
self-signed CA chain that does). `internal_tls.enabled=true` with an empty
`server_name` fails **loudly at startup** (`mtls.ClientConfig` errors with a fix
hint) rather than as a cryptic "ServerName must be specified" on the first dial.
`saslLogin` and `quotaStatus` now also carry an `internalTLS` block/mount — every
component that dials an internal service needs the cert mounted.

The director **ring** dial is separate — see below.

#### Ring mTLS — `director_service.ring_tls_server_name` (#753)

The director dials its ring peers (JOIN, right-neighbor, seed polls) by
**ephemeral pod IP**. Under `internal_tls` the dial verifies the peer's
certificate against `ring_tls_server_name` (a stable name), *not* the pod IP —
without it Go would check the pod IP against the cert's SANs and fail closed, so
the ring would silently never converge. The dial uses a proper client config
(with `RootCAs`); the ring *listener* keeps the mTLS server config.

The chart issues a **director-specific** internal-tls certificate (enable
`components.director.internalTLS.certificate`) whose SANs default to
`<release>-director-ring` and `<release>-director`, and renders
`ring_tls_server_name` to `<release>-director-ring` automatically. The stock
*shared* internal-tls secret has no such SAN — enabling `internal_tls` on the
director with that shared cert fails ring handshakes. On misconfiguration the
director is loud: an **ERROR** when `internal_tls` is on and peers are set but
`ring_tls_server_name` is empty, and a **Warn** when the configured name is
absent from its own loaded certificate.

**Certificate rotation.** cert-manager renews the Secret and kubelet refreshes
the mounted files, but each director loads its `tls.Config` **once at startup** —
a renewed cert is picked up only after a **rolling restart of the director
StatefulSet**. The ring survives a rolling restart normally (verified by the
#770 graceful-leave gates). Lazy in-process reload (`GetClientCertificate`) is a
possible future enhancement, not implemented here.

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

### Liveness vs readiness — why `yarilo-locks` has no watchdog self-check (#904)

The split, stated plainly so it is not "completed" back into a dependency probe:

- **liveness** = *local process state*. It restarts a process that is up but whose own request
  path is wedged.
- **readiness** = *Redis reachability*. It removes a pod from rotation when its backend is
  unreachable, and returns it automatically once Redis answers.

`yarilo-locks` in **remote mode deliberately wires no liveness self-check**, because it has no
local state that could wedge independently of Redis. The grant path is
`handleLock → RedisBackend.Acquire → Redis Lua script`, directly: `RedisBackend` holds no mutex,
no queue, no in-process shard — the acquire *is* the Redis round-trip. The only in-process grant
mutex is `MemoryBackend.mu`, and that backend is used **only** in embedded/test mode, never in
k8s. So the "up but locally wedged" state a liveness self-check exists to catch does not exist
here — there is nothing between accept and the Redis call to deadlock.

A self-check that round-trips Redis (the shape the #904 issue first sketched for this component)
is **rejected**: it re-creates the exact failure the watchdog guard-rail forbids. A hung Redis
(TCP alive, no replies) would hang that probe on **every** locks replica at once, restarting the
whole HA pool synchronously into a backoff loop — and a restart cannot fix a hung Redis. Both a
down Redis and a hung Redis already have the correct answer: the readiness ping hangs or fails,
the pod leaves rotation without a restart, and rejoins the moment Redis responds. Not-ready is
the right degradation; dead is not.

The dead-accept-loop and hard-crash cases are covered by `os.Exit(1)` on `Serve` error, no probe
involved. So the coverage is complete without a locks watchdog: crash → `os.Exit`; Redis
unreachable/hung → readiness; and there is no third, local-only failure mode to add.

**Revisit trigger.** If remote-mode `yarilo-locks` ever grows a genuine local stage in the grant
path — an in-process cache, a grant queue, an in-process shard — this decision is reopened: that
stage *can* wedge independently of Redis, and it should then get a liveness self-check that
acquires and releases a **probe-scoped** lock through that stage, bounded by a timeout well above
the worst-case acquire, and never reaching Redis. Until such a stage exists, there is nothing
local to probe.

---

## Standalone deployment — single-node k8s, scale-out by replicaCount

The standalone deployment targets a single k8s node, no director, no per-tag sharding. It is
**built to scale from 1 → N replicas of each component by editing values.yaml** — no rewiring,
no protocol changes, no code touched. The same pattern carries the operator from a one-pod dev
cluster to a small multi-replica production setup.

### Single-host, no Kubernetes — Docker Compose

For local development, evaluation and small self-hosted installs there is a
**Docker Compose** deployment ([deploy/compose/](../deploy/compose),
[DOCKER-COMPOSE.md](DOCKER-COMPOSE.md)). It collapses the topology further: the
whole server runs as **one `yarilo` process** in `mode: single` — every protocol
plus embedded auth, anvil and locks (in-memory) in-process, no login proxies and
no `yarilo-locks` service. The minimal profile needs no external dependencies
(SQLite userdb + local volumes); a `full` profile adds MariaDB and Redis. It is a
single-host target and is **not** highly available — for HA/scale-out use the
Helm standalone or backend deployments above.

### Components (all as k8s Deployments unless noted)

| Component | Default replicas | Scale by |
|:---|:---|:---|
| `yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login` | 1 each | `replicaCount` per protocol (login is stateless beyond TLS state) |
| `yarilo-lmtp-login` | 0 (disabled by default) | `components.lmtpLogin.enabled: true` in values.yaml; MTA-facing LMTP proxy — anvil CONNECT per recipient, SESSION token, preamble fan-out |
| `yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp` | 1 each | `replicaCount` per protocol (coordination via locks) |
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
across the matching login pods. Port `24` is a `Service` in front of `yarilo-lmtp-login`
(when enabled) which fans out per-recipient preamble connections to `yarilo-lmtp` backends.
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
helm/yarilo-director      → director pool
helm/yarilo-backend       → backend pool (one release per tag = per NFS shard, with its own locks)
```

### yarilo-shared
- `Deployment yarilo-auth` — replicaCount=2, stateless (userdb in an external SQL/LDAP).
- `Deployment yarilo-anvil` — replicaCount=2, state in Redis.
- `Deployment redis` (or external) — state backend for anvil.
- A ClusterIP Service for each.

### yarilo-director
- `Deployment yarilo-director` — replicaCount=3, self-organizing ring (#750): each
  replica dials only its right neighbor in `(ip, port)` sort order, never a full
  mesh, and every member count (including N=1, a lone replica) is a fully valid,
  service-serving state. Plain Deployment, not a StatefulSet — no per-pod stable
  network identity is needed. The headless `-director-ring` Service (#748/#751)
  is the join seed: its DNS name resolves directly to every ready pod's IP (not
  a single virtual IP), so any resolver hit lands on a live replica to
  `DIRECTOR-JOIN` against; membership is self-maintaining from there.
- 4 login-proxy processes (`yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-login`) — in separate containers or under a master-supervised process tree.
- ClusterIP Service — public entry point: :993/:995/:587/:24.
- ClusterIP Service (`-director-ring`) — internal ring protocol port; also the join seed.

### yarilo-backend (one release per tag, e.g. `yarilo-backend-a`)
**One co-located StatefulSet per tag** — the pod runs all four protocol containers plus the registration sidecar:

- `StatefulSet yarilo-backend-<tag>` — replicaCount=N. Pod containers:
  `yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`,
  `yarilo-managesieve`, `yarilo-fts` (`fts_addr = localhost`), and
  `yarilo-backend-reg` (registration sidecar). All share the pod IP.
- `Deployment yarilo-locks-<tag>` — replicaCount=2, cross-pod write coordination.
- `Deployment redis-<tag>` (or shared Redis) — state backend for locks.
- `Deployment yarilo-quota-status-<tag>` — global-read Postfix policy service,
  own replicaCount, stable ClusterIP for the external MTA (NOT in the pod;
  `backend-api` IS in the pod — see the co-location criterion above).
- One **PVC NFS (RWX)** — shared by all pods within the tag.
- One Headless Service — stable per-pod DNS for sticky routing from the director.

**Why co-located, not per-protocol StatefulSets:**
- **Routing coherence (the whole point).** Consistent hashing cannot give both
  "1 user = 1 pod across protocols" *and* independent per-protocol pod pools — they
  are mathematically incompatible. Separate pools would hash the same user to
  *different* pods for IMAP vs LMTP, so every cross-protocol write (LMTP delivery →
  the mailbox an IMAP session on another pod holds) crosses pods and fights over
  `yarilo-locks` / index-cache coherence. Co-locating restores the reference's
  one-mail-host invariant and makes the single ring + single userDir **correct**,
  not merely a shortcut (this is the causal fix for #788, not a workaround).
- **FTS single-writer for free (#675/#676).** The per-user Xapian index has the same
  single-writer requirement as the mailbox. Co-locating `yarilo-fts` (localhost)
  puts the user's index write handle on their already-sticky pod, so each per-user
  index on the shared NFS has exactly one writer cluster-wide — no fts ring, no
  ClusterIP round-robin corruption. One invariant ("1 user = 1 pod") fixes #788 and
  #675/#676 together.
- **One IP per user.** The director stores one pod IP per user; that IP serves every
  protocol, so no protocol dimension is needed on the ring, the userDir, or the wire.
- Vertical sizing stays per-protocol — each **container** keeps its own CPU/RAM
  limits. What is given up is **independent per-protocol replica-count / HPA**: you
  scale the pod (all protocols together), not one protocol's pool. This is a
  deliberate trade of an unused scaling axis for routing correctness.

**Trade-off (documented, accepted):** the whole pod scales as a unit, and one
unready protocol container takes the pod (all protocols) out of the ring — see the
readiness-gate semantics under "backend liveness". Cross-protocol writes still go
through `yarilo-locks` for cross-*pod* coordination (a user's replicas across the
tag), but same-user same-protocol traffic is pinned to one pod.

### Director routing — one ring

The director maintains **one** consistent-hashing ring and **one** user→pod
directory: `MD5(user) → yarilo-backend-<tag>-N` (the pod IP). The same pod answers
IMAP, POP3, Submission and LMTP for that user; the login proxy dials the
protocol-specific port on the resolved pod IP. `tag` selects the NFS shard, never
the protocol. There is no per-protocol ring — a single user never lands on
different pods for different protocols.

**Admin per-user ops reach the owning pod the same way (#792).** `yarilo-backend-api`
listens on the pod IP only, so a per-user admin op (`yarctl backend fts
rescan <user>`, folder/quota/index/… on a user) must hit the user's pod.
`yarctl` does the routing **client-side**: it asks the director
`GET /api/director/map?user=X` — which resolves with the *same* precedence a
login LOOKUP uses (sticky userDir pin → ring hash, the director owning the
assignment; #708 removed the separate admin-override map) — then dials
`http://{pod-ip}:{backend_api_port}`. It never picks
a pod itself: a client-side choice would race a concurrent login and put two pods
on one user's FTS index, breaking the single-writer invariant. Routing auto-enables
when a director URL is configured (co-located); with no director it uses the fixed
`--backend-url` (standalone). `--route-by-user=false` is a debug escape hatch for
talking to one pod directly. If the director is unreachable the op fails with a
clear error — never a silent fall-through to a random pod. Global ops (dict,
user iterate) carry no user and always use the fixed URL.

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

## Sizing per backend tag (one co-located StatefulSet + one NFS share + local locks)

| Parameter | Typical | Min/Max |
|:---|:---|:---|
| yarilo-backend replicaCount (co-located pod) | 3–5 | 1 / scale-out |
| per-container resources (imap/pop3/submission/lmtp) | sized independently within the pod | see "Sizing per backend pod" |
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

**Ring:** `MD5(username) → co-located backend pod` (within a single tag). One ring,
one pod IP per user, serving every protocol — the login proxy dials the
protocol-specific port on that IP.
**Tag assignment:** a separate user → tag map (admin-defined or hash-based shard) —
`tag` = NFS shard, not protocol.

1. Client → director's login proxy (TLS terminate).
2. Login proxy: passdb in-process (auth only: password + allow_nets), then userdb for home/mail. Backend receives user info via extended XCLIENT, skips passdb/userdb.
3. Director: determines the user's tag; the ring maps user → pod.
4. Director connects directly to the pod via stable DNS (headless Service).
5. Passes auth state in the preamble, proxies plain TCP.

`userDir` in the director is an in-memory cache of active user → pod mappings.
Synced between directors over the peer protocol.

---

## HA strategy

| Layer | HA approach |
|:---|:---|
| Director | replicaCount=3, self-organizing ring (#750) |
| Backend per tag | replicaCount=3–5, shared NFS RWX, ring rebalance |
| yarilo-locks (per tag) | replicaCount=2, state in Redis |
| yarilo-auth | replicaCount=2, stateless |
| yarilo-anvil | replicaCount=2, state in Redis |
| Redis | external HA (Sentinel/Cluster) or managed |
| NFS server | a separate HA effort (Pacemaker+DRBD, or managed NFS such as AWS EFS) |

**Backend failover sequence:**
1. The pod stops heartbeating — the `yarilo-backend-reg` sidecar sends BACKEND-DOWN
   on SIGTERM (immediate); a hard kill or any unready protocol container → the
   sidecar goes silent → TTL expiry (#776). Either way the **whole pod** leaves the
   ring for all protocols.
2. Director removes it from the ring.
3. Locks on the dead pod expire via TTL (30 s) on `yarilo-locks-<tag>`.
4. Ring rehash → users move to neighbouring replicas in the same tag.
5. The k8s scheduler brings up a new pod (~30 s).
6. The new pod mounts the same NFS and starts accepting connections.

---

## Initial placement policy — `director_service.assignment_policy` (#797)

Only the **first** (unpinned) user→backend assignment consults the policy; sticky
pins and USER-MOVE are unaffected. The director is the **single
owner** of placement — the LOOKUP path, LMTP `RouteUser`, and the admin
per-user resolve (#792) all funnel through one `assignAndPin`, so no caller can
independently pick a pod and split a user's per-user writer (#788).

- **`hash`** (default, the reference semantics): the consistent-hash backend for the
  user's tag. Deterministic — if a sticky entry expires after long inactivity the
  user returns to the same pod (warm per-user index locality on the shared PV,
  easy debugging).
- **`least_sessions`**: the least-loaded Up backend in the tag by a two-level,
  capacity-normalized load — **level 1** the requesting protocol's sessions,
  **level 2** total sessions among level-1 ties; each `count*100/vhosts`
  (`vhosts` 1..100; **`vhosts: 0` = drain → excluded**); tie-break lower
  `(ip, port)`. Session counts come from the live SESSION-OPEN/CLOSE registry
  (no new plumbing), which now carries a trailing `proto` field on LOOKUP /
  SESSION-OPEN. The admin resolve has no protocol → level 1 is skipped, total
  load decides. Session counts are replicated ring-wide (SESSION-OPEN/CLOSE gossiped as (origin, seq) envelopes, #804), so whichever random replica answers a LOOKUP decides on the full cluster view — not just the sessions on the watch-holding replica.

**Trade-offs (least_sessions).** It gives up hash determinism: an expired-then-
returning user may land on a different pod than before. Acceptable because the
data lives on the shared tag PV, but it must be a deliberate operator choice —
hence the `hash` default. Second, because a fresh resolve now **pins**, a bulk
admin sweep over never-logged-in users (e.g. `yarctl backend fts rescan`
across a cohort) creates userDir entries for each — a deliberate side effect (the
pins TTL-expire), so don't be surprised by a populated userDir after a mass
operation. Reference parity: the reference does hash + vhosts weighting only;
`least_sessions` is a yarilo extension, default stays reference-compatible.

---

## USER-MOVE and pin longevity (#708)

An admin **USER-MOVE** (`yarctl director users <u> move …`) is an
**operational tool — "shift this user off that backend now" — not permanent
routing configuration.** It writes a normal, TTL'd userDir pin at the target
(replicated ring-wide via the same `USER-MOVED` gossip) and immediately kicks
the user's sessions on the old backend so the next connection lands on the new
one. There is **no permanent-override map** (removed in #708) and **no
USER-RELEASE** — a move just expires.

**Longevity is session-driven, not permanent.** While the user holds a live
proxied session, the director keeps the pin's TTL fresh: every `user_expire/2`
it touches the pin of every user with an active session in the replicated
session registry (#804). Each director refreshes from its own ring-replicated
session view, so no extra propagation is needed. Once the user's **last session
closes**, the touches stop and the pin **lapses back to the ring hash** after
`director_service.user_expire`. So a move survives exactly as long as the user
stays connected; an idle user deterministically returns to their hash backend.

Operators who need a **permanent** placement should use **tags** (a user's
tag-pool is routing configuration), not a move.

---

## Kick pacing — `user_kick_delay` and `max_parallel_kicks` (#740)

Two knobs shape how sessions are torn down, mirroring the reference's
`director_user_kick_delay` / `director_max_parallel_kicks`:

- **`director_service.user_kick_delay`** (default `2`s) — an admin-initiated
  kick (director API, typically the tail of a user move) waits this long before
  the `USER-KICKED` push, so an in-flight command on the old backend can
  complete. Applies to the **admin path only**: a backend-down / lease-expiry
  kick fires immediately (there is nothing to grace on a dead backend), and the
  split-writer conflict-kick is never delayed. Negative = disabled (immediate).

- **`director_service.max_parallel_kicks`** (default `100`) — when a backend
  goes down, its sessions are kicked in batches of this size with a short pause
  between batches, spreading the re-login stampede across the surviving backends
  instead of firing every kick at once. Negative = no batching.

**Migrating from `dovecot.conf`:** `director_max_parallel_moves` has **no yarilo
equivalent and is intentionally omitted**. yarilo rehashes lazily — a moved or
kicked user is re-placed only on its next `LOOKUP` (kick → re-login → LOOKUP),
so there is no proactive bulk-move phase to bound; the move rate is already
capped by `max_parallel_kicks`. A parsed-but-unread key would be a config gap,
so the key does not exist rather than existing as a no-op.

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

This is fundamentally different from the reference C model (fork per user → process per user →
~10 MB per session). yarilo holds 1000+ users in a single process without resource overhead.
