# yarilo — Architecture

This document is the **authoritative reference** for yarilo's architecture.
All development decisions must be consistent with what is written here.
If something in the code contradicts this document — the code is wrong.

---

## Core principles

- **Security** — each component runs with minimum required permissions; all inter-component traffic is mTLS.
- **Process lightness** — each component does one thing; goroutines per session within each process.
- **Fault tolerance** — crash of one Pod does not affect others; k8s restarts failed Pods automatically.
- **Scalability** — stateless components scale horizontally via HPA; stateful components use director affinity.
- **Isolation** — k8s securityContext + NetworkPolicy provides component isolation; no cross-component storage access.

---

## Deployment model

yarilo is a **multi-binary** system. Each component is a separate compiled binary deployed as a separate
k8s workload (Deployment for stateless components, StatefulSet for sticky-routed and peer-syncing components).
There is no monolithic binary. There is no master process.

Each binary handles its role via goroutines — one goroutine per connection/session within the process.

**Infrastructure topology is defined in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) and SVG schemas
([docs/yarilo_director.svg](docs/yarilo_director.svg), [docs/yarilo_backend.svg](docs/yarilo_backend.svg),
[docs/yarilo_standalone.svg](docs/yarilo_standalone.svg)).** Those are the source of truth for k8s
resource types, scaling, sharding, and inter-component coordination.

### Binary layout

```
/usr/lib/yarilo/
  yarilo-imap-login           # TLS terminator + proxy (in director deployment)
  yarilo-pop3-login           # ditto
  yarilo-submission-login     # ditto
  yarilo-lmtp-proxy           # MTA-facing TCP proxy (in director deployment)
  yarilo-imap                 # IMAP session backend (in backend deployment)
  yarilo-pop3                 # POP3 session backend
  yarilo-submission           # Submission session backend
  yarilo-lmtp                 # LMTP delivery backend
  yarilo-auth                 # passdb + userdb (shared service)
  yarilo-auth-worker
  yarilo-anvil                # connlimit + session counters (shared service)
  yarilo-director             # ring + userDir + monitor
  yarilo-locks                # cross-pod write coordination (per backend tag)
  yarilo-monitor              # sidecar in director pod — polls backend pod health, reports to director ring
```

k8s replaces infrastructure processes:

| Component | Replaced by |
|:---|:---|
| log daemon | stdout → k8s log collection (fluentd / loki) |
| config daemon | ConfigMap mounted as file |
| master / supervisor | k8s Deployment restart policy |
| stats daemon | `/metrics` per Pod, Prometheus ServiceMonitor |

### Source layout

```
app/
  yarilo-imap-login/main.go
  yarilo-imap/main.go
  yarilo-pop3-login/main.go
  yarilo-pop3/main.go
  yarilo-submission-login/main.go
  yarilo-submission/main.go
  yarilo-lmtp/main.go
  yarilo-auth/main.go
  yarilo-auth-worker/main.go
  yarilo-anvil/main.go
  yarilo-director/main.go
  yarilo-monitor/main.go
internal/
  login/imap/      — TLS accept + SASL + TCP proxy goroutine
  login/pop3/
  login/submission/
  imap/            — IMAP session server (goroutines per connection)
  pop3/
  submission/
  lmtp/
  auth/            — passdb/userdb chain
  anvil/           — connection accounting
  director/        — consistent hash ring, user→pod routing
  monitor/         — backend pod health checks, lock TTL liveness reports to director
pkg/
  mailbox/         — MailboxBackend + IndexBackend interfaces
  config/          — YAML config via koanf
helm/
  yarilo-shared/   — shared services (yarilo-auth, yarilo-anvil, Redis)
    Chart.yaml
    values.yaml
    templates/
      auth-deployment.yaml
      anvil-deployment.yaml
      redis-statefulset.yaml
  yarilo-director/ — director pool (login-proxies + director StatefulSet + monitor sidecar)
    Chart.yaml
    values.yaml
    templates/
      imap-login-deployment.yaml
      pop3-login-deployment.yaml
      submission-login-deployment.yaml
      lmtp-proxy-deployment.yaml
      director-statefulset.yaml   — 3 pods peer-sync
  yarilo-backend/  — backend pool (один release на tag, 4 StatefulSet-и per protocol)
    Chart.yaml
    values.yaml      — per-protocol replicaCount + HPA config
    templates/
      imap-statefulset.yaml
      pop3-statefulset.yaml
      submission-statefulset.yaml
      lmtp-statefulset.yaml
      locks-deployment.yaml
      nfs-pv.yaml                 — per-tag NFS share
```

---

## Helm chart structure

**Three charts**, кожен deployment-шар окремо. Сторонній storage (NFS, Redis HA) — поза yarilo-чартами.

```sh
# Раз на інсталяцію — shared infrastructure services
helm install yarilo-shared ./helm/yarilo-shared -f values-prod.yaml

# Раз на інсталяцію — director pool
helm install yarilo-director ./helm/yarilo-director -f values-prod.yaml

# Один release на tag — backend pool з власним NFS shard
helm install yarilo-backend-a ./helm/yarilo-backend --set tag=a -f values-prod.yaml
helm install yarilo-backend-b ./helm/yarilo-backend --set tag=b -f values-prod.yaml
# ...
```

### values-prod.yaml (per chart) — приклад

**yarilo-shared:**
```yaml
auth:
  replicas: 2
anvil:
  replicas: 2
  redis:
    address: redis.shared.svc:6379
```

**yarilo-director:**
```yaml
director:
  replicas: 3      # peer-sync ring, фіксований
imapLogin: { replicas: 2 }
pop3Login: { replicas: 2 }
submissionLogin: { replicas: 2 }
lmtpProxy: { replicas: 2 }
```

**yarilo-backend (per tag):**
```yaml
tag: a
imap:
  replicas: 3
  hpa: { minReplicas: 3, maxReplicas: 10, metric: connCount }
pop3:
  replicas: 1
  hpa: { minReplicas: 1, maxReplicas: 3, metric: pollRate }
submission:
  replicas: 2
  hpa: { minReplicas: 2, maxReplicas: 5, metric: outboundRate }
lmtp:
  replicas: 3
  hpa: { minReplicas: 3, maxReplicas: 15, metric: deliveryQueue }
locks:
  replicas: 2
nfs:
  server: nfs-a.storage.svc
  path: /export/yarilo-a
  size: 5Ti
```

All pod labels include `app.kubernetes.io/part-of: yarilo` for cluster-wide log tailing:

```sh
stern -l app.kubernetes.io/part-of=yarilo
```

---

## k8s workloads

### yarilo-shared chart

| Workload | Type | Service | Replicas | Notes |
|:---|:---|:---|:---|:---|
| `yarilo-auth` | Deployment | ClusterIP :9100 | 2+ | stateless, HPA, userdb queries external SQL/LDAP |
| `yarilo-anvil` | Deployment | ClusterIP :9101 | 2 | state в Redis (HA), conn+session counters |
| `redis-shared` | StatefulSet (or external) | ClusterIP :6379 | per-Redis-HA-design | state backend для anvil |

### yarilo-director chart

| Workload | Type | Service | Replicas | Notes |
|:---|:---|:---|:---|:---|
| `yarilo-director` | StatefulSet | Headless :9102 + ClusterIP :9103 (admin API) | 3 | peer-sync ring, monitor sidecar per pod |
| `yarilo-monitor` | sidecar | (in director pod) | 1 per director | polls backends, marks down in ring |
| `yarilo-imap-login` | Deployment | LoadBalancer :993 / :143 | 2+ | TLS terminator + proxy, HPA |
| `yarilo-pop3-login` | Deployment | LoadBalancer :995 / :110 | 2+ | HPA |
| `yarilo-submission-login` | Deployment | LoadBalancer :465 / :587 | 2+ | HPA |
| `yarilo-lmtp-proxy` | Deployment | ClusterIP/NodePort :24 | 2+ | MTA-facing, IP allowlist via NetworkPolicy |

### yarilo-backend chart (один release на tag)

| Workload | Type | Service | Replicas | Notes |
|:---|:---|:---|:---|:---|
| `yarilo-backend-<tag>-imap` | StatefulSet | Headless :10993 | N (HPA) | sticky ring per pod, NFS RWX |
| `yarilo-backend-<tag>-pop3` | StatefulSet | Headless :10110 | M (HPA) | sticky ring per pod, NFS RWX |
| `yarilo-backend-<tag>-submission` | StatefulSet | Headless :10587 | P (HPA) | sticky ring per pod, NFS RWX |
| `yarilo-backend-<tag>-lmtp` | StatefulSet | Headless :10024 | Q (HPA) | sticky ring per pod, NFS RWX |
| `yarilo-locks-<tag>` | Deployment | ClusterIP :9104 | 2 | cross-pod write coord, state в Redis |
| `redis-<tag>` | StatefulSet (or shared) | ClusterIP :6379 | 1+ | state backend для locks |
| NFS PV `<tag>` | PV/PVC | — | RWX | shared всіма 4 StatefulSet-ами в tag-у |

**Чому StatefulSet для backend і director:**
- Director: peer-sync ring потребує stable identity (`director-0`, `director-1`, `director-2`) для початкового discovery
- Backend session-процеси: director routes user → конкретний pod через stable DNS (`backend-a-imap-2.headless.svc`), потрібен StatefulSet з headless Service для stable pod names

**Чому 4 окремі StatefulSet-и на протокол замість 1 StatefulSet з 4 контейнерами:**
- Independent scaling — POP3 типово 1 pod, LMTP при mass-delivery 10+ pods
- Process isolation — crash одного протоколу не зачіпає інші
- Right-sized resources — кожен з власними CPU/RAM limits та HPA-метрикою

**Trade-off:** Cross-protocol writes (LMTP delivery + IMAP STORE на той же mailbox) → cross-pod координація через `yarilo-locks`. Locks — critical path для всіх writes.

### Security context per workload

| Workload | runAsUser | Capabilities | Storage |
|:---|:---|:---|:---|
| `yarilo-imap-login` | `dovenull` | NET_BIND_SERVICE | none |
| `yarilo-pop3-login` | `dovenull` | NET_BIND_SERVICE | none |
| `yarilo-submission-login` | `dovenull` | NET_BIND_SERVICE | none |
| `yarilo-lmtp-proxy` | `dovenull` | NET_BIND_SERVICE | none |
| `yarilo-imap` | `yarilo` | none | RWX PVC (NFS) |
| `yarilo-pop3` | `yarilo` | none | RWX PVC (NFS) |
| `yarilo-submission` | `yarilo` | none | RWX PVC (NFS, для Sent folder) |
| `yarilo-lmtp` | `yarilo` | none | RWX PVC (NFS) |
| `yarilo-auth` | `yarilo` | none | none |
| `yarilo-anvil` | `yarilo` | none | none |
| `yarilo-director` | `yarilo` | none | none |
| `yarilo-monitor` (sidecar) | `yarilo` | none | none |
| `yarilo-locks` | `yarilo` | none | none |

---

## Connection lifecycle

### IMAP (port 993)

```
client ──TLS:993──► yarilo-imap-login (dovenull)
                        │ TLS handshake
                        │ SASL auth ──mTLS──► yarilo-auth :9100
                        │ conn limit ──mTLS──► yarilo-anvil :9101
                        │ routing   ──mTLS──► yarilo-director :9102
                        │                      → returns yarilo-imap pod address
                        │ FAIL → close
                        │ OK:
                        │   dial yarilo-imap pod :10993 (plain TCP, internal ClusterIP)
                        │   goroutine: proxy TLS conn ↔ TCP conn
                        │
                    yarilo-imap (yarilo uid)
                        │ accepts plain TCP from imap-login
                        │ goroutine per connection
                        │ maildir access via RWX PVC
                        │ on disconnect → goroutine exits
                        │
                    yarilo-imap-login (still running, TLS proxy goroutine)
                        read TLS → write TCP → yarilo-imap
                        read TCP → write TLS → client
                        goroutine exits when TCP conn closes
```

### POP3 / Submission

Same pattern as IMAP. Login pod proxies to session pod via plain TCP after auth.

### LMTP (port 24)

LMTP is proxied through yarilo-director to ensure delivery reaches the backend that
owns the recipient's mailbox (consistent-hash affinity, same as IMAP/POP3).

```
MTA ──TCP:24──► yarilo-director (yarilo uid)
                    │ read LMTP preamble (extract recipient username)
                    │ ring lookup → yarilo-lmtp pod address
                    │ dial yarilo-lmtp pod :10024 (plain TCP, internal ClusterIP)
                    │ goroutine: proxy TCP conn ↔ TCP conn
                    │
                yarilo-lmtp (yarilo uid)
                    auth lookup ──mTLS──► yarilo-auth :9100
                    goroutine per delivery
                    write to maildir via RWX PVC
```

LMTP has no login phase — trusted MTAs connect directly to the director's ClusterIP or
NodePort (protected by network policy; not exposed via LoadBalancer).

---

## Service communication (mTLS RPC)

Між компонентами використовується **mTLS TCP** через k8s Services (не класичний IPC через pipes/Unix sockets — це RPC).
Plain TCP — лише на data plane між director-проксі і backend-pod-ом всередині trust boundary (ClusterIP + NetworkPolicy).

| From | To | Transport | Protocol |
|:---|:---|:---|:---|
| `*-login` | `yarilo-auth` | mTLS TCP :9100 | TAB-delimited (INTERNALS.md §3) |
| `*-login` | `yarilo-anvil` | mTLS TCP :9101 | TAB-delimited |
| `*-login` | `yarilo-director` | mTLS TCP :9102 | TAB-delimited (INTERNALS.md §2) |
| `*-login` | `yarilo-imap/pop3/submission` | plain TCP ClusterIP | raw protocol bytes (proxy) |
| `yarilo-director` | `yarilo-lmtp` | plain TCP ClusterIP | raw LMTP bytes (proxy) |
| `yarilo-monitor` (sidecar) | backend `/healthz` of each StatefulSet pod | mTLS HTTP | health polling (rebalance ring on failures) |
| `yarilo-lmtp` | `yarilo-auth` | mTLS TCP :9100 | TAB-delimited |
| `yarilo-imap/pop3/submission/lmtp` | `yarilo-locks-<tag>` | mTLS TCP :9104 | TAB-delimited (LOCK/UNLOCK/RENEW) |
| `yarilo-imap/pop3/submission/lmtp` | `yarilo-auth` | mTLS TCP :9100 | TAB-delimited (userdb) |
| `yarilo-imap/pop3/submission/lmtp` | `yarilo-anvil` | mTLS TCP :9101 | TAB-delimited (SESSION events) |

---

## mTLS

All internal TCP services (auth, anvil, director, locks, health) require mutual TLS.
Every pod presents a certificate; the peer verifies it against the internal CA.
Connections without a valid certificate are rejected.

### Certificate provisioning

cert-manager issues certificates per Deployment via `Certificate` resources:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: yarilo-auth
spec:
  secretName: yarilo-auth-tls
  issuerRef:
    kind: ClusterIssuer
    name: yarilo-internal-ca
  dnsNames:
    - yarilo-auth.yarilo.svc.cluster.local
  duration: 24h
  renewBefore: 6h
```

Internal CA is a self-signed ClusterIssuer managed by cert-manager.
All pod TLS configs reference the same CA bundle for peer verification.

### Go TLS config pattern

**Server (e.g. yarilo-auth):**
```go
tlsCfg := &tls.Config{
    Certificates: []tls.Certificate{cert},
    ClientAuth:   tls.RequireAndVerifyClientCert,
    ClientCAs:    caPool,
    MinVersion:   tls.VersionTLS13,
}
```

**Client (e.g. yarilo-imap-login calling auth):**
```go
tlsCfg := &tls.Config{
    Certificates: []tls.Certificate{cert},
    RootCAs:      caPool,
    ServerName:   "yarilo-auth.yarilo.svc.cluster.local",
    MinVersion:   tls.VersionTLS13,
}
```

Certificates and CA bundle mounted from k8s Secrets into each pod at:
```
/etc/yarilo/tls/tls.crt
/etc/yarilo/tls/tls.key
/etc/yarilo/tls/ca.crt
```

Paths configurable via `values.yaml` — never hardcoded.

---

## Storage

Maildir requires shared filesystem for `yarilo-imap`, `yarilo-pop3`, `yarilo-lmtp`:

- **CephFS** (preferred) — distributed, no SPOF, native k8s CSI via `rook-ceph`
- **NFS** — simpler, single NFS server

`yarilo-director` ensures user→pod affinity: the same user always routes to the same pod
under normal operation. On pod failure, director reroutes to another pod that can access
the same maildir via RWX PVC.

```yaml
persistence:
  accessMode: ReadWriteMany
  storageClass: cephfs   # or nfs
```

---

## Graceful shutdown

On SIGTERM (sent by k8s on Pod termination):

```
SIGTERM received by pod
  │
  ├─ login pods: stop accepting new connections immediately
  ├─ active sessions: wait up to sessionGracePeriod for current command to finish
  ├─ after sessionGracePeriod: close remaining sessions with "server shutting down"
  └─ after killTimeout: k8s sends SIGKILL
```

All timing parameters in `helm/values.yaml` — never hardcoded:

```yaml
shutdown:
  sessionGracePeriod: 60   # seconds
  killTimeout: 10          # seconds
```

`terminationGracePeriodSeconds` computed in Helm template:

```yaml
terminationGracePeriodSeconds: {{ add .Values.shutdown.sessionGracePeriod .Values.shutdown.killTimeout 20 }}
```

---

## Logging standard

All processes write structured JSON via `log/slog` to stdout.
k8s collects stdout and forwards to log aggregation (fluentd / loki).
`LOG_LEVEL=debug` enables debug output — no code changes needed.

### Guiding principle

Follow Dovecot's log semantics: what is logged, when, and which fields.
Format is JSON (slog), information content mirrors Dovecot exactly.

### Session ID

Generated at connection accept time in the login process.

```
sessionID = base64( microseconds[48bit] | remote_port[16bit] | remote_ip_bytes )
```

Stored as plain base64 string in JSON (no angle brackets).

### slog field names

| Field | Type | Description |
|:---|:---|:---|
| `process` | string | binary name: `yarilo-imap-login`, `yarilo-imap`, … |
| `pid` | int | OS process ID |
| `version` | string | yarilo version (startup log only) |
| `user` | string | authenticated username (`alice@example.com`) |
| `session` | string | session ID (base64) |
| `method` | string | SASL mechanism: `PLAIN`, `LOGIN`, `OAUTH2` |
| `rip` | string | effective remote IP — after HAProxy/XCLIENT resolution |
| `rport` | int | effective remote port |
| `lip` | string | effective local IP |
| `lport` | int | effective local port |
| `pxip` | string | physical TCP peer IP (only when differs from `rip`) |
| `pxport` | int | physical TCP peer port (only when differs from `rport`) |
| `tls` | bool | true when TLS or HAProxy-terminated TLS |
| `tls_cipher` | string | cipher suite |
| `in` | int | bytes received from client during session |
| `out` | int | bytes sent to client during session |
| `err` | string | error string |

### Log events

**Startup:**
```json
{"level":"INFO","process":"yarilo-imap-login","pid":1,"msg":"yarilo v0.3.11 starting","version":"0.3.11","lip":"::","lport":993}
```

**Auth failure:**
```json
{"level":"INFO","process":"yarilo-imap-login","pid":1,"msg":"Login failed","user":"alice@example.com","method":"PLAIN","rip":"203.0.113.5","rport":61234,"lip":"10.0.0.1","lport":993,"tls":true,"session":"abc123XY","err":"authentication failed"}
```

**Login success:**
```json
{"level":"INFO","process":"yarilo-imap-login","pid":1,"msg":"Login","user":"alice@example.com","method":"PLAIN","rip":"203.0.113.5","rport":61234,"lip":"10.0.0.1","lport":993,"tls":true,"session":"abc123XY"}
```

**Session operation:**
```json
{"level":"INFO","process":"yarilo-imap","pid":1,"user":"alice@example.com","session":"abc123XY","msg":"SELECT INBOX","messages":142,"unseen":3}
```

**Disconnect:**
```json
{"level":"INFO","process":"yarilo-imap","pid":1,"user":"alice@example.com","session":"abc123XY","msg":"Disconnected: Logged out","in":1234,"out":56789}
```

**LMTP delivery:**
```json
{"level":"INFO","process":"yarilo-lmtp","pid":1,"msg":"delivery accepted","from":"sender@other.com","to":"alice@example.com","size":4096,"rip":"10.0.0.3","session":"xyz789AB"}
```

### IP resolution rules

1. Physical TCP peer IP captured at accept time → initial `rip`/`rport`.
2. HAProxy PROXY header present → `rip`/`rport` = client IP; `pxip`/`pxport` = physical peer.
3. XCLIENT command received → `rip`/`rport` updated; `pxip`/`pxport` = physical peer.
4. Neither → `rip`/`rport` = physical peer; `pxip`/`pxport` omitted.

### Implementation rule

Login process: create `slog.With("rip", ..., "lip", ..., "tls", ...)` at accept time.
Session process: create `slog.With("user", ..., "session", ...)` after auth.
Every log call uses the base logger — never log without connection/session context.

---

## Known issues and required fixes

### Cross-process file locking — storage corruption risk

**Problem:** `internal/storage/mailbox/maildir` and `internal/storage/index/file` use `sync.Mutex`
for in-process concurrency. `sync.Mutex` does not protect against concurrent access from separate
pods (`yarilo-imap` and `yarilo-lmtp`) on the shared NFS/CephFS volume.

| File | Risk |
|:---|:---|
| `yarilo-uidlist` | UID assignment race → duplicate UIDs or corruption |
| fileindex (`*.idx`) | concurrent writes → index corruption |

Raw mail delivery (`rename()` into `new/`) is safe — atomic at OS level.

**Required fix:** Replace `sync.Mutex` with `fcntl` advisory exclusive lock (`syscall.Flock`)
on `yarilo-uidlist` and index files at every write. NFSv4 and CephFS both support POSIX locking.

**Status:** Not yet implemented. Must be done before multi-process mode ships.

---

## Security model

| Threat | Mitigation |
|:---|:---|
| Exploit in TLS/SASL handling | `yarilo-imap-login` runs as `dovenull`, no PVC access, NetworkPolicy blocks storage pods |
| Cross-pod unauthorized access | mTLS on all internal services — certificate required |
| MITM between pods | mTLS with internal CA verification |
| Cross-user maildir access | Each session pod runs as `yarilo` uid; NetworkPolicy; director affinity prevents concurrent access |
| Auth bypass | `yarilo-auth` reachable only via mTLS; NetworkPolicy restricts access to login pods |
| Connection flooding | `yarilo-anvil` enforces `max_userip_connections` globally across all login replicas |
| Backend failure | `yarilo-monitor` (sidecar in director pod) detects via `/healthz` polling, `yarilo-director` removes from ring, reroutes in-flight connections |
