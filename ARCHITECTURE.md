# yarilo — Architecture

This document is the **authoritative reference** for yarilo's architecture.
All development decisions must be consistent with what is written here.
If something in the code contradicts this document — the code is wrong.

---

## Core principles

- **Security** — each component runs with minimum required permissions; all inter-process traffic is mTLS.
- **Process lightness** — each component does one thing; goroutines per session within each process.
- **Fault tolerance** — crash of one Pod does not affect others; k8s restarts failed Pods automatically.
- **Scalability** — stateless components scale horizontally via HPA; stateful components use director affinity.
- **Isolation** — k8s securityContext + NetworkPolicy provides component isolation; no cross-component storage access.

---

## Deployment model

yarilo is a **multi-binary** system. Each component is a separate compiled binary deployed as a separate
k8s Deployment. There is no monolithic binary. There is no master process.

Each binary handles its role via goroutines — one goroutine per connection/session within the process.

### Binary layout

```
/usr/lib/yarilo/
  yarilo-imap-login
  yarilo-imap
  yarilo-pop3-login
  yarilo-pop3
  yarilo-submission-login
  yarilo-submission
  yarilo-lmtp
  yarilo-auth
  yarilo-auth-worker
  yarilo-anvil
  yarilo-director
  yarilo-health
  yarilo-ipc
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
  yarilo-health/main.go
  yarilo-ipc/main.go
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
  health/          — backend health probes
  ipc/             — inter-process command routing
pkg/
  mailbox/         — MailboxBackend + IndexBackend interfaces
  config/          — YAML config via koanf
helm/
  yarilo/          — single Helm chart for all components
    Chart.yaml
    values.yaml    — all component config in one file
    templates/
      imap-login/  — Deployment, Service, HPA per component
      imap/
      pop3-login/
      pop3/
      ...
```

---

## Helm chart

**One chart, one release.** All components deployed via a single `helm install yarilo ./helm/yarilo`.

```sh
helm install yarilo ./helm/yarilo -f values-prod.yaml
helm upgrade yarilo ./helm/yarilo -f values-prod.yaml
```

Each component is independently configurable and can be enabled/disabled in `values.yaml`:

```yaml
components:
  imapLogin:
    enabled: true
    replicas: 2
    image:
      tag: ""        # defaults to Chart.appVersion
  imap:
    enabled: true
    replicas: 2
  auth:
    enabled: true
    replicas: 2
  anvil:
    enabled: true
    replicas: 1
  director:
    enabled: true
    replicas: 2
  # ...
```

All pod labels include `app.kubernetes.io/part-of: yarilo` to allow cluster-wide log tailing:

```sh
stern -l app.kubernetes.io/part-of=yarilo
```

---

## k8s Deployments

| Deployment | Service | Replicas | Notes |
|:---|:---|:---|:---|
| `yarilo-auth` | ClusterIP :9100 | 2+ | stateless, HPA |
| `yarilo-anvil` | ClusterIP :9101 | 1 | shared conn state, single instance |
| `yarilo-director` | ClusterIP :9102 | 2–3 | ring state, leader election |
| `yarilo-health` | ClusterIP :9103 | 1–2 | polls backends, notifies director |
| `yarilo-ipc` | ClusterIP :9104 | 1 | kick-user, admin command bus |
| `yarilo-imap-login` | LoadBalancer :993 / :143 | 2+ | stateless, HPA |
| `yarilo-pop3-login` | LoadBalancer :995 / :110 | 2+ | stateless, HPA |
| `yarilo-submission-login` | LoadBalancer :465 / :587 | 2+ | stateless, HPA |
| `yarilo-imap` | ClusterIP :10993 | 2+ | NFS/CephFS RWX, director affinity |
| `yarilo-pop3` | ClusterIP :10110 | 2+ | NFS/CephFS RWX, director affinity |
| `yarilo-submission` | ClusterIP :10587 | 2+ | stateless relay, HPA |
| `yarilo-lmtp` | ClusterIP :10024 | 2+ | NFS/CephFS RWX |

### Security context per Deployment

| Deployment | runAsUser | Capabilities | Storage |
|:---|:---|:---|:---|
| `yarilo-imap-login` | `dovenull` | NET_BIND_SERVICE | none |
| `yarilo-pop3-login` | `dovenull` | NET_BIND_SERVICE | none |
| `yarilo-submission-login` | `dovenull` | NET_BIND_SERVICE | none |
| `yarilo-imap` | `yarilo` | none | RWX PVC |
| `yarilo-pop3` | `yarilo` | none | RWX PVC |
| `yarilo-lmtp` | `yarilo` | none | RWX PVC |
| `yarilo-submission` | `yarilo` | none | none |
| `yarilo-auth` | `yarilo` | none | none |
| `yarilo-anvil` | `yarilo` | none | none |
| `yarilo-director` | `yarilo` | none | none |
| `yarilo-health` | `yarilo` | none | none |
| `yarilo-ipc` | `yarilo` | none | none |

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

## Inter-process communication

All inter-pod communication uses **mTLS** (mutual TLS). Plain TCP is used only between
a login pod and its target session pod within the same trust boundary (ClusterIP, NetworkPolicy enforced).

| From | To | Transport | Protocol |
|:---|:---|:---|:---|
| `*-login` | `yarilo-auth` | mTLS TCP :9100 | TAB-delimited (INTERNALS.md §3) |
| `*-login` | `yarilo-anvil` | mTLS TCP :9101 | TAB-delimited |
| `*-login` | `yarilo-director` | mTLS TCP :9102 | TAB-delimited (INTERNALS.md §2) |
| `*-login` | `yarilo-imap/pop3/submission` | plain TCP ClusterIP | raw protocol bytes (proxy) |
| `yarilo-director` | `yarilo-lmtp` | plain TCP ClusterIP | raw LMTP bytes (proxy) |
| `yarilo-health` | `yarilo-director` | mTLS TCP :9102 | TAB-delimited |
| `yarilo-lmtp` | `yarilo-auth` | mTLS TCP :9100 | TAB-delimited |
| admin | `yarilo-ipc` | mTLS TCP :9104 | TAB-delimited |

---

## mTLS

All internal TCP services (auth, anvil, director, health, ipc) require mutual TLS.
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
| Backend failure | `yarilo-health` detects, `yarilo-director` removes from ring, reroutes in-flight connections |
