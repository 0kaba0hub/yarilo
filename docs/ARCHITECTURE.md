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

## Two rules the ACL/naming series proved

A run of defects through the ACL, namespace and folder-name code all had one
shape. Closing them taught two rules worth stating so the next reader starts
from the right question rather than rediscovering them. The examples name things
that **were removed** in the course of those fixes, not code you will find
today — that is the point of the first rule.

### One idea, one implementation

Every defect in that series sat where **two implementations of one idea
disagreed**: four constructions of a namespace's storage context; two owners of
NFC normalisation (a driver step and the path builder); no single entry per ACL
identifier; two definitions of "owner" (by namespace type and by person); two
meanings of the insert right (IMAP vs delivery); two write paths for an ACL
(a client read-modify-write and the server). None was a logic error in either
copy — each was defensible alone; the bug was that they drifted.

**When you find a defect here, do not ask which branch is wrong. Ask where the
two implementations of one idea are.** Then close the gap one of two ways:

- **remove one of them**, when there can be a single implementation;
- **make one an owner and guard the rest**, when the idea has many call sites
  that cannot collapse into one function — `mailbox.NormalizeName` is called
  from every namespace resolver and stays the only place NFC happens, held there
  not by deletion but by an AST guard that fails if a resolver forgets it. Reach
  for a guard only when removal is genuinely impossible; a guard is a promise the
  compiler cannot keep, so it is second choice.

The fixes that closed the *class* rather than the *instance* each did one of
these, never a reconciliation of the two copies:

- `insertRight(spec)` was **deleted**, not corrected — the predicate that chose
  the right by namespace type was itself the defect; IMAP now always requires
  the insert right (#1119).
- `FolderSubpathForm`'s NFC parameter was **removed**, not typed — a path
  builder must not decide the name form; normalisation moved to the one owner,
  `mailbox.NormalizeName`, at the name-entry boundary (#1113).
- `adminCheckPRc` was **deleted** — it carried the admin path's own, weaker
  resolution of the `a` right, and the fix was a call to the same
  `requireAdminOn` the IMAP path uses, not a repair of its logic. It is the
  sharpest case, and the one to remember: the second implementation was not
  merely drifting, it was **letting a peer escalate** — `SETACL` in a shared
  namespace was ungated because its private owner-check compared the session
  user against themselves. Two implementations of one idea are not always a
  cosmetic cost (#1107, #1108).
- the CLI's `get`→edit→`set` helpers were **erased**, not canonicalised —
  leaving them would keep a second, unlocked way to write an ACL, and the next
  author reaches for what already exists (#1114).

A corollary, made checkable: "one implementation" means *complete*, not merely
*single*. A shared constructor with fewer fields than the one it did not absorb
is worse than two explicit ones, because it looks authoritative. `NamespaceUserInfo`
first omitted `Groups`/`ACLUser`/`ACLGroups` — exactly the fields the LMTP
construction carried — so wiring LMTP to it, the obvious next step, would have
**silently switched group ACLs off at delivery**. The check is: compare the
field set against the *most complete* of the constructions you are absorbing,
not the first (#1109, #1115).

### A test's fixture must distinguish the two behaviours

Several defects that week survived review because a test passed. Each had a
fixture whose inputs could not tell the correct behaviour from the broken one,
so it stayed green under both and read as coverage: an escape-matrix row with no
combining mark after the escaped byte, an FTS-root check with only one account,
an IMAP normalisation test on a filesystem (APFS) that composed the name itself,
an ACL evaluator case with no lower-tier negative, a smoke check that reused a
cached session, an APPEND test whose client framed the literal correctly and so
never hit the tail the server mishandled.

**A test that cannot fail under the defect it names asserts nothing. Choose the
one input that separates the two behaviours, and make the non-distinguishing
case visible rather than absent.** The rule holds on the *assertion* as much as
the input: `if ui.indexDir(composed) != ui.indexDir(composed)` shipped in a test
whose fixture four lines above already guarded that the two spellings differ —
the lesson applied to the input and not to the comparison, which staticcheck
(SA4000) caught because the expression is identical on both sides. Watch the
assertion as closely as the fixture.

The mechanisms now in the tree are three shapes of this:

- the `wasa` column in the ACL evaluator matrix records what the old code
  answered, so a row where `wasa == want` is visibly proving nothing;
- a fixture that cannot distinguish in one environment declares it — the
  end-to-end normalisation test carries a `t.Skip` when the filesystem composes
  names, rather than vanishing from the CI runners where it works;
- where a rule is enforced by hand across many sites, an AST guard turns "N
  things to remember" into one that fails — the folder-normalisation guard over
  the admin decode boundaries, the dispatch-normalisation guard, and the
  `FolderSubpath`-signature guard that keeps the removed NFC parameter from
  returning.

Both rules are one idea at different layers: a thing defined in two places
drifts, whether the thing is an implementation or the input a test claims to
cover.

---

## Deployment model

yarilo is a **multi-binary** system. Each component is a separate compiled binary deployed as a separate
k8s workload (Deployment for stateless components, StatefulSet for sticky-routed and peer-syncing components).
There is no monolithic binary. There is no master process.

Each binary handles its role via goroutines — one goroutine per connection/session within the process.

**Infrastructure topology is defined in [DEPLOYMENT.md](DEPLOYMENT.md) and the SVG schemas
below.** Those are the source of truth for k8s resource types, scaling, sharding, and
inter-component coordination.

#### Director deployment (login proxies + director ring → backend tags)

<img src="https://raw.githubusercontent.com/yarilomail/yarilo/main/docs/yarilo_director.svg" width="100%" alt="Director deployment topology"/>

#### Backend deployment (one co-located StatefulSet per tag)

<img src="https://raw.githubusercontent.com/yarilomail/yarilo/main/docs/yarilo_backend.svg" width="100%" alt="Backend deployment topology"/>

#### Standalone deployment (full stack in one pod)

<img src="https://raw.githubusercontent.com/yarilomail/yarilo/main/docs/yarilo_standalone.svg" width="100%" alt="Standalone deployment topology"/>

> **Note (#908):** the diagrams draw `yarilo-warden` as a single shared service for
> clarity. Its replica count is a config value, not a fixed topology: with
> `state_backend: redis` warden runs N replicas behind the same ClusterIP (all state
> — penalty, sessions, kick bus — in shared Redis), and with `state_backend: memory`
> it stays a single pod. See *Scaling `yarilo-warden`* in
> [docs/DEPLOYMENT.md](DEPLOYMENT.md).

The logical request flow (login → session → shared services → storage):

```
  Internet
     |
     | IMAPS :993 / IMAP :143 / POP3S :995 / POP3 :110
     | LMTP :24 / ManageSieve :4190 / SASL :4190
     v
+------------------------+  +------------------------+
|  yarilo-imap-login     |  |  yarilo-pop3-login     |  login pods (TLS termination,
|  yarilo-lmtp-login     |  |  yarilo-managesieve-   |  HAProxy / XCLIENT, passdb,
|  yarilo-submission-    |  |  login                 |  warden rate-limit, fd-passing)
|  login / sasl-login    |  +------------------------+
+------------------------+
         |  Unix fd-passing (SCM_RIGHTS) after auth
         v
+------------------------+  +------------------------+
|  yarilo-imap           |  |  yarilo-pop3           |  session pods (mailbox,
|  yarilo-lmtp           |  |  yarilo-managesieve    |  index, Sieve execution,
|  yarilo-submission     |  |  yarilo-backend-api    |  quota, ACL)
+------------------------+  +------------------------+
         |
         | cross-process write coordination (TCP mTLS)
         v
+------------------------+  +------------------------+
|  yarilo-locks          |  |  yarilo-auth           |  shared services
|  yarilo-warden          |  |  Redis (dict / locks)  |
+------------------------+  +------------------------+
         |
         | NFS PV (RWX) — shared by all session pods in a tag
         v
  [ Mailbox + Index files ]
```

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
  yarilo-warden                # connlimit + session counters + kick bus (shared; Redis Pub/Sub across replicas, #908)
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
  yarilo-warden/main.go
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
  warden/           — connection accounting
  director/        — consistent hash ring, user→pod routing
  monitor/         — backend pod health checks, lock TTL liveness reports to director
pkg/
  mailbox/         — MailboxBackend + IndexBackend interfaces
  config/          — YAML config via koanf
helm/
  yarilo-shared/   — shared services (yarilo-auth, yarilo-warden, Redis)
    Chart.yaml
    values.yaml
    templates/
      auth-deployment.yaml
      warden-deployment.yaml
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
warden:
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
| `yarilo-warden` | Deployment | ClusterIP :9101 | 2 | state в Redis (HA), conn+session counters |
| `redis-shared` | StatefulSet (or external) | ClusterIP :6379 | per-Redis-HA-design | state backend для warden |

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
| `yarilo-imap-login` | `nobody` | NET_BIND_SERVICE | none |
| `yarilo-pop3-login` | `nobody` | NET_BIND_SERVICE | none |
| `yarilo-submission-login` | `nobody` | NET_BIND_SERVICE | none |
| `yarilo-lmtp-proxy` | `nobody` | NET_BIND_SERVICE | none |
| `yarilo-imap` | `yarilo` | none | RWX PVC (NFS) |
| `yarilo-pop3` | `yarilo` | none | RWX PVC (NFS) |
| `yarilo-submission` | `yarilo` | none | RWX PVC (NFS, для Sent folder) |
| `yarilo-lmtp` | `yarilo` | none | RWX PVC (NFS) |
| `yarilo-auth` | `yarilo` | none | none |
| `yarilo-warden` | `yarilo` | none | none |
| `yarilo-director` | `yarilo` | none | none |
| `yarilo-monitor` (sidecar) | `yarilo` | none | none |
| `yarilo-locks` | `yarilo` | none | none |

---

## Connection lifecycle

### IMAP (port 993)

```
client ──TLS:993──► yarilo-imap-login (nobody)
                        │ TLS handshake
                        │ speak IMAP pre-auth (CAPABILITY / AUTHENTICATE / LOGIN)
                        │ collect username + password from client
                        │ AUTH request ──mTLS──► yarilo-auth :9100
                        │   passdb chain, brute-force penalty via yarilo-warden
                        │   → returns session token (64-char hex, one-time, TTL 60s)
                        │ check nologin / allow_nets from auth response
                        │ routing ──mTLS──► yarilo-director :9102
                        │   → returns yarilo-imap pod address
                        │ conn limit ──mTLS──► yarilo-warden :9101
                        │ FAIL → send NO to client, close
                        │ OK:
                        │   send tag OK to client
                        │   dial yarilo-imap pod :10993 (plain TCP, internal ClusterIP)
                        │   XCONN XCLIENT ADDR=<clientIP> SESSION=<id> TOKEN=<tok> USER=<user>
                        │   goroutine: proxy TLS conn ↔ TCP conn
                        │
                    yarilo-imap (yarilo uid)
                        │ accepts plain TCP from imap-login
                        │ receives XCONN XCLIENT preamble (ADDR/SESSION/TOKEN/USER)
                        │ VERIFY(token, user=<username>) ──mTLS──► yarilo-auth :9100
                        │   → validates token AND checks token was issued to <username>
                        │   → token consumed (one-time); returns confirmed username
                        │ USER(username) ──mTLS──► yarilo-auth :9102 (master socket)
                        │   → userdb lookup: home, mail, quota, groups, …
                        │ enters pre-authenticated IMAP state
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

## Auth architecture rules

**passdb and userdb are strictly separate — never call userdb from the login process.**

```
login pod   →  AUTH(user, pass)          →  yarilo-auth :9100  (passdb only → token)
                                                                  ↑ NO userdb here
backend pod →  VERIFY(token, user=<u>)   →  yarilo-auth :9100  (token + username binding check)
backend pod →  USER(<username>)          →  yarilo-auth :9102  (master socket → userdb → home/mail/quota/groups)
```

Rules that must never be violated:

- **Login pods call passdb only.** `RunAuth` must not invoke `userdb.Lookup`. The auth result from a login-side AUTH carries only: `username`, `token`, `nologin`, `allow_nets`. No `home`, no `mail`.
- **Session pods call userdb.** After VERIFY succeeds, every session binary (imap, pop3, submission, managesieve, lmtp) calls `USER <username>` on the master socket (:9102) to obtain `home`, `mail`, `quota_rule`, `groups`, and any extra fields. This is the only source of storage identity.
- **VERIFY binds token to username.** The VERIFY command includes the username from the preamble (`VERIFY\t<id>\t<token>\tuser=<u>`). yarilo-auth rejects with FAIL if the token was not issued to that username. This prevents token reuse across users.
- **`RunAuth` = passdb chain only.** `WithAuthenticatorUserdb` / userdb wiring must not be attached to the login-side authenticator.

---

## Service communication (mTLS RPC)

Між компонентами використовується **mTLS TCP** через k8s Services (не класичний IPC через pipes/Unix sockets — це RPC).
Plain TCP — лише на data plane між director-проксі і backend-pod-ом всередині trust boundary (ClusterIP + NetworkPolicy).

| From | To | Transport | Protocol |
|:---|:---|:---|:---|
| `*-login` | `yarilo-auth` | mTLS TCP :9100 | TAB-delimited AUTH — **passdb only** → session token |
| `*-login` | `yarilo-warden` | mTLS TCP :9101 | TAB-delimited (connection counting) |
| `*-login` | `yarilo-director` | mTLS TCP :9102 | TAB-delimited LOOKUP |
| `*-login` | `yarilo-imap/pop3/submission` | plain TCP ClusterIP | XCLIENT preamble (ADDR/SESSION/TOKEN/USER), then raw protocol bytes (proxy) |
| `yarilo-imap/pop3/submission/managesieve` | `yarilo-auth` | mTLS TCP :9100 | TAB-delimited VERIFY(token, user=) — token + username binding check |
| `yarilo-imap/pop3/submission/lmtp/managesieve` | `yarilo-auth` | mTLS TCP :9102 (master) | TAB-delimited USER — **userdb lookup** → home, mail, quota, groups |
| `yarilo-director` | `yarilo-lmtp` | plain TCP ClusterIP | raw LMTP bytes (proxy) |
| `yarilo-monitor` (sidecar) | backend `/healthz` of each StatefulSet pod | mTLS HTTP | health polling (rebalance ring on failures) |
| `yarilo-imap/pop3/submission/lmtp` | `yarilo-locks-<tag>` | mTLS TCP :9104 | TAB-delimited (LOCK/UNLOCK/RENEW) |
| `yarilo-imap/pop3/submission/lmtp` | `yarilo-warden` | mTLS TCP :9101 | TAB-delimited (SESSION events) |

---

## mTLS

All internal TCP services (auth, warden, director, locks, health) require mutual TLS.
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

## Third-party Go library patches

When yarilo needs a feature from `github.com/emersion/go-imap/v2` (or any
upstream Go library) that has not yet shipped, we maintain a fork at
`github.com/0kaba0hub/<lib>` with a two-branch layout:

- **upstream-mirror branch** (e.g. `v2`) — verbatim mirror of upstream.
  Fast-forward only.
- **`yarilo-patches`** — mirror + cherry-picked downstream commits. yarilo's
  `go.mod` `replace` directive pins to a specific commit on this branch.

The fork's `YARILO_PATCHES.md` (default branch view) documents the layout,
tracking workflow, and exit plan. When upstream merges the original PR, we
drop the `replace` directive and the patches branch.

**Current pins:**

| Library | Reason | Tracking PR |
|:---|:---|:---|
| `github.com/emersion/go-imap/v2` | Server-side CONDSTORE + QRESYNC (RFC 7162) for Phase IMAP-E | [emersion/go-imap#756](https://github.com/emersion/go-imap/pull/756) |
| `github.com/emersion/go-imap/v2` | Server-side METADATA (RFC 5464) for Phase IMAP-G | [emersion/go-imap#717](https://github.com/emersion/go-imap/pull/717) |

`go.mod` keeps the upstream import path so storage-layer code reads
naturally; only the `replace` line points elsewhere.

---

## Dict abstraction

`pkg/dict` is yarilo's general key-value store. Every feature that needs
durable per-user or per-mailbox metadata — RFC 5464 METADATA, quota
counters, ACL state, sieve script indices, future replication cursors —
sits on top of this contract instead of inventing its own storage.

A single interface (`Dict` + `Tx` + `Iterator`) is implemented by
multiple drivers; concrete storage (a local JSON file for standalone,
Redis for shared cluster state, PostgreSQL for operators who already
run one) is selected via config, not code. Adding a new dict-backed
feature does not touch this package.

### Contract

| Surface | Type |
|:---|:---|
| `dict.Dict` | `Lookup` / `Iterate` / `Begin` / `ExpireScan` / `Wait` / `Close` / `Name` |
| `dict.Tx` | `Set` / `Unset` / `AtomicInc` / `Commit` / `Rollback` |
| `dict.Iterator` | `Next` / `Key` / `Values` / `Err` / `Close` |
| Constants | `PathPrivate = "priv/"`, `PathShared = "shared/"` reserved key prefixes |
| Flags | `IterRecurse`, `IterSortByKey`, `IterSortByValue`, `IterNoValue`, `IterExactKey` |
| Result | `CommitOK`, `CommitNotFound` (atomic-inc on missing key), `CommitFailed`, `CommitWriteUncertain` (remote write race) |
| Op-settings | `OpSettings.Username` / `HomeDir` / `ExpireSecs` / `NoSlownessWarning` / `HideLogValues` |

Helpers in the same package: `Escape` / `Unescape` for `'/'` and `'%'`
inside key path components, and `MemoryTx` — a buffered transaction
for drivers without native atomic multi-key writes.

### Drivers (in-tree)

| Driver pkg | Production-ready | Notes |
|:---|:---|:---|
| `pkg/dict/memory` | tests / dev only | in-process map; no persistence; lazy TTL expiry |
| `pkg/dict/fail` | wiring placeholder | every op returns `ErrFailDriver`; used to wire "feature disabled" |
| `pkg/dict/file` | standalone deployment | JSON envelope, atomic temp-file + rename, in-process sync.RWMutex; NOT safe across processes |
| `pkg/dict/redis` | backend k8s deployment | `go-redis/v9`; SET/GET/DEL, MULTI/EXEC tx, INCRBY, EXPIRE, SCAN; prefix-isolated |
| `pkg/dict/sql` | backend k8s deployment | `database/sql` + `modernc.org/sqlite` (pure Go) + `pgx/v5/stdlib`; per-namespace table with `expires` column + index |

Driver authors implement the three interfaces and call `dict.Register(name, init)`
from their package's `init()`. The `pkg/dict/drivers/all` package
blank-imports all in-tree drivers — binaries that want them all just
`import _ "github.com/yarilomail/yarilo/pkg/dict/drivers/all"`.

### Path expansion

`pkg/dict/varexpand` performs `%`-variable substitution for templated
path / prefix strings used by dict drivers:

| Verb | Meaning |
|:---|:---|
| `%u` | full username (`alice@example.com`) |
| `%n` | local-part (`alice`) |
| `%d` | domain (`example.com`); empty when no `@` |
| `%h` | home dir |
| `%i` | numeric uid as text |
| `%%` | literal `%` |

Callers expand templates BEFORE `dict.Open` — Open takes literal paths
for the file driver and literal prefixes for redis/sql.

### Config

`pkg/config.Config.Dicts` is a `map[string]DictConfig` of named dicts.
Yarilo features look them up by name (`cfg.Dicts["metadata"]`):

```yaml
dicts:
  metadata:
    driver: file
    settings:
      path: "/var/yarilo/dicts/metadata.json"

  quota:
    driver: redis
    settings:
      addr: "yarilo-redis.yarilo.svc.cluster.local:6379"
      db: 0
      prefix: "yarilo:quota:"
    expire_secs: 86400
```

`expire_secs`, `username`, `home_dir` at the top of a `DictConfig` are
defaults for `OpSettings` that the caller can override per-op.

### CLI

`yarctl dict <command>` exposes the full surface for ops debugging:
`lookup`, `iterate`, `set`, `unset`, `atomic-inc`, `expire-scan`,
`commit-batch` (stdin TAB-delimited script), `drivers`. The dict is
selected either via `--config PATH --dict NAME` (production config) or
ad-hoc via `--driver` + repeated `--setting key=value` (debugging). See
[DICT.md](DICT.md) for the full reference.

### Deferred from this phase

- Standalone dict-server / dict-proxy daemon — yarilo uses redis/sql
  for cross-pod sharing, so a separate proxy daemon is not currently
  needed.
- LDAP / CDB read-only drivers — niche, add when a customer asks.
- Async callback API — `context.Context` cancellation already covers
  the cancellation use cases.

---

## Namespaces

yarilo follows the IMAP RFC 2342 / RFC 9051 §6.3.10 namespace model
and the standard operational shape for it:

| Class | Wire | What it carries |
|:---|:---|:---|
| **Personal** | `("" "/")` | The user's own mailboxes (INBOX + everything created via CREATE). |
| **Other Users** | `("user/" "/")` | Read/write access to another user's mailboxes, gated by ACL. |
| **Shared** | `("Shared/" "/")` | Folders shared between groups or all users, gated by ACL. |
| **Public** (variant of Shared) | `("Public/" "/")` | Folders accessible to every authenticated user. |

Each namespace MAY use its own separator (permitted by the model; we follow it).

### Phase ordering

| Phase | Delivers |
|:---|:---|
| **NS-1a** (`v1.20`) | Wire-protocol: `NAMESPACE` response driven by `cfg.Namespaces[]`. Only Personal carries real mailboxes. |
| **NS-1b** (`v1.21`, shipped) | Per-namespace storage routing via in-session `nsHandle` dispatch keyed on namespace prefix. Each implemented namespace opens its own `UserMailbox` + `UserIndex` + per-namespace `subscriptions-<ns>` file at login. `LIST` traverses every implemented namespace (personal first, then by prefix). `SELECT`/`STATUS`/`APPEND`/`COPY`/`MOVE`/`SUBSCRIBE` route by prefix. METADATA `/private/*` on shared/public mailboxes embeds a SHA-256 hash of the accessing user in the dict key (`priv/box/<guid>/u-<userhash>/<entry>`) so users do not see each other's private annotations on the same folder; `/shared/*` stays global. Other Users (`user/<owner>/...`) is declared in the wire spec but `SELECT` under it returns `NO`. |
| **ACL-1** | RFC 4314 — required for Shared / Other Users / Public to be actually usable (without it any user reads anyone's stuff). Enforcement primitives shipped (#490); namespace-aware LMTP/Sieve delivery + POST-right shipped (#503/#504). |
| **NS-2 (owner-templated)** | Owner-templated shared / other namespaces (`prefix: user/%u/`, `location: maildir:%h`): the location variables expand against the **owner** (userdb lookup), and the `nsHandle` is built **on demand per owner** and cached per session. Owner-tier ACL: the owner's own session has implicit full rights; a peer is gated by the owner's ACL. **Same farm tag only** — resolves the owner's storage when the owner's mailbox carries the same farm tag (same PV) as the session's mailbox; a different-farm owner is NS-3. Works in standalone and single-farm backend. Design: [OWNER_SHARED_NS.md](OWNER_SHARED_NS.md) (#499 item 3). |
| **NS-3** | Director routing: when accessing `user/alice/*` and alice's mailbox carries a **different farm tag** (its data is on a PV the accessing pod does not mount), route just the owner-access leg to a pod in alice's farm (cross-pod RPC or namespace-pinned pool). Same-farm access (incl. standalone and single-farm backend) works without this. NS-2 fails closed (`NO` / LMTP implicit-keep) when the owner is on a different farm tag, until NS-3 lands (#499 item 4). |

### Storage layout (post-NS-1b)

```
/var/mail/vhosts/<domain>/<user>/        ← personal, per-user (existing)
  Maildir/
    .index/
    cur/ new/ tmp/

/var/yarilo/shared/                      ← shared, one root per install (or per-tag for backend deploy)
  marketing/
    announcements/
      .index/ cur/ new/

/var/yarilo/public/                      ← public, analogous
  announcements/...
```

Per-namespace backends are constructed at backend startup, one
`MailboxBackend` + `IndexBackend` pair per namespace; sessions hold
the namespace dispatcher and route every mailbox operation through it
based on the `prefix:` match.

### Quota: owner-paid

When QUOTA-1 lands: storage consumed in `user/alice/INBOX` counts
against alice's quota, not the accessing user's. Public / Shared
namespaces use their own system-wide quota root (declared in the
`quota:` config block).

### What lives in dict

Folder-attached state (METADATA, ACL, replication cursors) is keyed by
the rename-stable folder GUID (`pkg/mailbox.Folder.GUID`), so RENAME
within a namespace and folder lifecycle in shared/public namespaces
do not invalidate any dict keys. METADATA `priv/` entries on shared
or public folders get an additional per-accessing-user dimension in
the dict key so users cannot read each other's private annotations
on a shared folder.

See [NAMESPACE.md](NAMESPACE.md) for the operator-facing
YAML schema and current limitations.

---

## Admin API plane

Operator HTTP admin splits along the two planes whose state it
exposes:

| Plane | Binary | Port | Surface |
|:---|:---|:---|:---|
| **Director** | `yarilo-director` | `:9103` `/api/director/...` | ring / backends / users / peers — routing topology |
| **Backend**  | `yarilo-backend-api` | `:9105` `/api/backend/<service>/...` | dict (today); acl / quota / folder / user / mailbox (future) |

Both speak JSON over HTTPS with Bearer-token auth + IP allow-list.
The `yarctl` CLI is a thin HTTP client over both — operator
runs `yarctl director <command>` or `yarctl backend
<service> <command>`. Each plane has its own URL + token
(`--url`/`--token` vs `--backend-url`/`--backend-token`); their
defaults work out of the box when running inside the respective
pod.

### Why two binaries

Director state (ring, peers, user-to-backend mapping) lives in
director's in-memory process state. Backend state (NFS-mounted
maildir, on-disk indices, dict instances) lives in backend
session-process pods. In a multi-pod backend deployment **these are
physically different pods on different nodes** — they cannot share
one HTTP server. The separation also keeps each plane's lifecycle
independent (rolling restart of director admin does not touch
storage ops, and vice versa).

In standalone single-pod deployment both binaries run in the same
pod; operator still hits the two HTTP endpoints separately. The
CLI's flag separation makes the routing decision explicit rather
than magic.

### Future services per plane

Each plane is a registry — services land under it as features
ship. Director can grow distributed-state services (e.g.
`/api/director/dict/userdb_cache/...` once director uses dict for
peer-sync) without changing the CLI shape; the same `dict`
subcommand sits under both planes when needed.

### Wire reference

[BACKEND-API.md](BACKEND-API.md) — backend-plane
endpoints (dict surface today; ACL / quota / folder added in
subsequent phases). [DIRECTOR-API.md](DIRECTOR-API.md)
— director plane.

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

Follow the reference log semantics: what is logged, when, and which fields.
Format is JSON (slog); the information content mirrors the reference implementation exactly.

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

### Cross-process write coordination — storage corruption risk

**Problem:** `internal/storage/mailbox/maildir` and `internal/storage/index/file` use `sync.Mutex`
for in-process concurrency. `sync.Mutex` does not protect against concurrent access from separate
processes (`yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`) — they share storage
but live in distinct address spaces. Affects both standalone (single pod, 4 processes, one PVC)
and backend (multi-pod StatefulSets, one NFS RWX) deployments.

| File | Risk |
|:---|:---|
| `yarilo-uidlist` | UID assignment race → duplicate UIDs or corruption |
| fileindex (`*.idx`) | concurrent writes → index corruption |

Raw mail delivery (`rename()` into `new/`) is safe — atomic at OS level.

**Required fix:** Route every cross-process write through **`yarilo-locks`** — the single locking
abstraction in `pkg/locks`. One wire protocol (TAB-delimited, see [DEPLOYMENT.md](DEPLOYMENT.md)
§yarilo-locks). Two backends behind one identical wire protocol:

| Use | `yarilo-locks` mode | Backend | Transport |
|:---|:---|:---|:---|
| **Every k8s Helm release (standalone or backend)** | `remote` | Redis (bundled or external) | mTLS TCP `:9104` |
| Unit tests / non-k8s CLI dev | `embedded` | in-memory map (ephemeral) | Unix socket |

Production k8s is always remote — Unix sockets cannot cross pods, so embedded mode breaks the
moment `replicaCount > 1`. Embedded stays in the binary for tests and CLI dev; it is never the
Helm default. The choice is config-driven (`locks_service.mode`) so the same compiled binary
serves both — see CLAUDE.md §Config-not-binary.

In-process goroutine concurrency keeps `sync.Mutex` as a fast-path before any `yarilo-locks` call —
the two-tier scheme avoids RTT for intra-process contention. `fcntl`/`flock` is not used: it has
no EVENT channel for IDLE notifications, opaque metrics, and shaky NFS semantics.

**Status:** `pkg/locks` foundation + `yarilo-locks` binary landed in v1.4.0 (Phase 0).
`internal/storage/mailbox/maildir` and `internal/storage/index/file` write paths wired
through `pkg/locks` in v1.5.0 (Phase 1): two-tier mutex + cross-process X lock on
`mbox:<user>:<folder>`, atomic `AllocateAppend` for UID assignment, integration test
proving no UID collisions and no uidlist corruption under concurrent two-process writes.
Backend wiring (config-driven `LocksClient` reach into `backend.New`) shipped in v1.6.0
(Phase 2.1). LMTP delivery and IMAP `APPEND`/`COPY`/`MOVE`/INBOX-rename swapped from
the race-prone `NextUID++` pattern to `AllocateAppend` in v1.7.0 (Phase 2.2).

---

## Security model

| Threat | Mitigation |
|:---|:---|
| Exploit in TLS/SASL handling | `yarilo-imap-login` runs as `nobody`, no PVC access, NetworkPolicy blocks storage pods |
| Cross-pod unauthorized access | mTLS on all internal services — certificate required |
| MITM between pods | mTLS with internal CA verification |
| Cross-user maildir access | Each session pod runs as `yarilo` uid; NetworkPolicy; director affinity prevents concurrent access |
| Auth bypass | `yarilo-auth` reachable only via mTLS; NetworkPolicy restricts access to login pods |
| Connection flooding | `yarilo-warden` enforces `max_userip_connections` globally across all login replicas |
| Backend failure | `yarilo-monitor` (sidecar in director pod) detects via `/healthz` polling, `yarilo-director` removes from ring, reroutes in-flight connections |
