# yarilo-monitor

`yarilo-monitor` is a sidecar container that runs alongside `yarilo-director`. It probes
backend pods directly via IMAP/POP3/LMTP login and reports health state changes to the
director (`BACKEND-FLUSH` on failure, `BACKEND-UP` on recovery).

---

## How it works

```
yarilo-monitor (sidecar, same pod as director)
    │
    │  1. Connect to director ring protocol (127.0.0.1:9102)
    │  2. Read HOST handshake → seed initial backend list
    │  3. Listen for RING-CHANGE pushes → add / remove backends
    │  4. For each backend IP:
    │       every `interval` seconds:
    │         probe enabled protocols (IMAP login / POP3 USER+PASS / LMTP LHLO)
    │         on failure × retry_count → rapid poll → BACKEND-FLUSH to director
    │         on recovery               → BACKEND-UP  to director
    │
    ▼
yarilo-director  →  ring.SetUp(ip, false/true)  →  RING-CHANGE broadcast to peers
```

Credentials are per-tag — the same tag label used on backends, users, and storage
identifies a monitoring account. All pod replicas in the same tag share one set of
credentials. An empty-string tag `""` covers untagged backends (fallback).

---

## Configuration

`yarilo-monitor` reads `/etc/yarilo/monitor.yaml`. The path can be overridden with the
`MONITOR_CONFIG` environment variable.

### `director_addr`

| Key | Default | Description |
|:---|:---|:---|
| `director_addr` | `"127.0.0.1:9102"` | Director ring protocol address (same pod → localhost). |

### Probe settings

| Key | Default | Description |
|:---|:---|:---|
| `poll_imap` | `true` | Probe each backend via IMAP LOGIN. |
| `imap_port` | `993` | Port to connect to for IMAP probes. |
| `poll_pop3` | `false` | Probe each backend via POP3 USER/PASS. |
| `pop3_port` | `110` | Port to connect to for POP3 probes. |
| `poll_lmtp` | `false` | Probe each backend via LMTP LHLO (no auth required). |
| `lmtp_port` | `24` | Port to connect to for LMTP probes. |
| `interval` | `10` | Seconds between probe rounds. |
| `timeout` | `3` | Seconds per individual probe attempt. |

### Failure detection

| Key | Default | Description |
|:---|:---|:---|
| `retry_count` | `3` | Consecutive failures before entering rapid poll. |
| `rapid_rounds` | `10` | Number of rapid poll iterations. |
| `rapid_fails_needed` | `7` | Failures in rapid poll required to declare backend down. |

After `retry_count` consecutive failures the monitor runs `rapid_rounds` quick probes.
If more than `rapid_fails_needed` of those fail, `BACKEND-FLUSH` is sent to the director.
When a subsequent probe succeeds on a flushed backend, `BACKEND-UP` is sent to restore it.

### Credentials (`tags`)

```yaml
tags:
  "":             # fallback for untagged backends
    user: monitor@example.com
    password: secret
  ssd:
    user: monitor-ssd@example.com
    password: ssd-secret
  hdd:
    user: monitor-hdd@example.com
    password: hdd-secret
```

The tag is looked up by the backend's ring tag. If no entry exists for the tag, the `""`
(empty string) entry is used as a fallback. If neither exists, credentials are empty and
the probe connects but skips the login step (useful for LMTP).

---

## Full example

```yaml
director_addr: "127.0.0.1:9102"

interval: 10
timeout: 3
retry_count: 3
rapid_rounds: 10
rapid_fails_needed: 7

poll_imap: true
imap_port: 993
poll_pop3: false
pop3_port: 110
poll_lmtp: false
lmtp_port: 24

tags:
  "":
    user: monitor@example.com
    password: secret
```

---

## Helm values (`components.director.monitor`)

| Helm value | Config key | Description |
|:---|:---|:---|
| `monitor.enabled` | — | Enable the sidecar container. |
| `monitor.image` | — | Image override (defaults to the main yarilo image). |
| `monitor.pollIMAP` | `poll_imap` | Enable IMAP probe. |
| `monitor.imapPort` | `imap_port` | IMAP probe port. |
| `monitor.pollPOP3` | `poll_pop3` | Enable POP3 probe. |
| `monitor.pop3Port` | `pop3_port` | POP3 probe port. |
| `monitor.pollLMTP` | `poll_lmtp` | Enable LMTP probe. |
| `monitor.lmtpPort` | `lmtp_port` | LMTP probe port. |
| `monitor.interval` | `interval` | Poll interval (seconds). |
| `monitor.timeout` | `timeout` | Per-probe timeout (seconds). |
| `monitor.retryCount` | `retry_count` | Failures before rapid poll. |
| `monitor.rapidRounds` | `rapid_rounds` | Rapid poll iterations. |
| `monitor.rapidFailsNeeded` | `rapid_fails_needed` | Rapid poll failure threshold. |
| `monitor.tags` | `tags` | Per-tag credentials map. |

---

## Prometheus metrics (director)

The director exposes backend health and session counts via `/metrics`:

### `yarilo_director_backend_info`

Ring membership gauge. Value is always `1`; the `status` label carries the state.

```
yarilo_director_backend_info{ip="10.0.0.1", port="993", tag="ssd", status="up"} 1
yarilo_director_backend_info{ip="10.0.0.2", port="993", tag="ssd", status="flush"} 1
```

| Label | Values | Description |
|:---|:---|:---|
| `ip` | — | Backend pod IP. |
| `port` | — | Backend ring port. |
| `tag` | — | Backend group tag. |
| `status` | `up` \| `flush` | `up` = accepting new sessions; `flush` = draining (health check failed). |

### `yarilo_director_backend_sessions`

Exact count of active proxied sessions per backend and client-facing protocol.
Incremented when `biProxy` starts, decremented when it returns.

```
yarilo_director_backend_sessions{ip="10.0.0.1", port="993", tag="ssd", protocol="imaps"} 17
yarilo_director_backend_sessions{ip="10.0.0.1", port="993", tag="ssd", protocol="imap"}  3
yarilo_director_backend_sessions{ip="10.0.0.1", port="993", tag="ssd", protocol="pop3s"} 2
```

| Label | Values | Description |
|:---|:---|:---|
| `ip` | — | Backend pod IP. |
| `port` | — | Backend ring port. |
| `tag` | — | Backend group tag. |
| `protocol` | `imap` \| `imaps` \| `pop3` \| `pop3s` \| `lmtp` | Client-facing protocol. |

Total sessions per backend (all protocols):
```promql
sum by (ip, port, tag) (yarilo_director_backend_sessions)
```

### Alert examples

```promql
# Backend not healthy
yarilo_director_backend_info{status!="up"} == 1

# Backend session count exceeds threshold
sum by (ip, port, tag) (yarilo_director_backend_sessions) > 500
```

---

## Prometheus metrics (login path) — #881

Before these existed, `yarilo-auth`, `yarilo-anvil` and the login pods exported
only Go runtime metrics, so a login stall could not be attributed to a
component without `kubectl exec` into cgroup files and log grepping. That gap
produced a wrong root cause in #878: auth was blamed on the strength of a
scrape-averaged CPU gauge while the real bottleneck was CFS throttling on the
login pod.

### `yarilo_login_phase_seconds`

**Start here.** Histogram of one login phase, per protocol. A login walks the
phases in order and each is a separate network dependency, so this single
metric names the owner of a stall instead of requiring a bisect across four
services.

```
yarilo_login_phase_seconds_bucket{protocol="imap", phase="auth_dial",  le="5.12"}  1841
yarilo_login_phase_seconds_bucket{protocol="imap", phase="auth",       le="0.128"} 1802
```

| `phase` | Covers |
|:---|:---|
| `tls_handshake` | Client-facing TLS termination (IMAPS/POP3S implicit TLS). |
| `preamble` | Pre-auth protocol exchange that collects credentials. |
| `auth_dial` | Opening the connection to `yarilo-auth` (a full mTLS handshake today). |
| `auth` | One `AUTH` round-trip. Observed per attempt — the retry loop reuses the connection. |
| `director_lookup` | Director `LOOKUP`, including confirmed-kick hold retries. |
| `anvil_connect` | Dial + `CONNECT` to `yarilo-anvil`. |
| `backend_dial` | Dial to the backend pod, including fast-fail re-route. |
| `backend_preamble` | Reading the backend greeting (token `VERIFY` happens here). |

`auth_dial` is observed on failure as well as success: a timed-out dial is the
most important latency sample there is, and dropping it would leave the
histogram looking healthy exactly when the path is broken.

Slowest phase over 5 minutes:
```promql
topk(3, histogram_quantile(0.99, sum by (phase, le) (rate(yarilo_login_phase_seconds_bucket[5m]))))
```

### `yarilo_login_result_total`

Login outcomes: `ok`, `unavailable`, `backend_rejected`, `preamble_error`,
`tls_error`. Kept as distinct series on purpose — collapsing `unavailable` into
a generic failure counter is what made the #878 storm hard to read.

### `yarilo_login_sessions`

Currently proxied sessions held open by this login pod, by protocol.

### `yarilo_auth_*`

| Metric | Type | Notes |
|:---|:---|:---|
| `request_seconds{verb,result}` | histogram | Wall-clock per verb (`AUTH`, `VERIFY`). **Includes** the deliberate penalty/policy/failure delays — it answers "how long did the login proxy wait", not "how much work did auth do". |
| `passdb_seconds{driver,result}` | histogram | One chain driver call. Compare against `request_seconds` to separate real backend cost from intentional tarpit. |
| `scheme_verify_seconds{scheme}` | histogram | Password-scheme cost alone. BCRYPT/SHA512-CRYPT are expensive by design; this separates a raised cost factor from a slow query. Emitted by whichever process verifies — auth for wire logins, session binaries for in-process auth. |
| `cache_lookups_total{result}` | counter | `hit` \| `miss` \| `expired` \| `pwd_mismatch`. The cache itself collapses the last three into one miss counter; only this split shows that the TTL is too short (`expired`) versus a stale credential being retried (`pwd_mismatch`). |
| `cache_entries` / `cache_bytes` / `cache_max_bytes` | gauge | Fill ratio. The cache is bytes-bounded, so a full cache silently degrades into a pass-through and every login pays the passdb round-trip again. |
| `connections` / `connections_total` | gauge / counter | Connection churn. `connections_total` rising in lockstep with the login rate means every login pays a fresh mTLS handshake; a flat curve means connections are being reused. |

Cache hit ratio:
```promql
sum(rate(yarilo_auth_cache_lookups_total{result="hit"}[5m]))
  / sum(rate(yarilo_auth_cache_lookups_total[5m]))
```

### `yarilo_anvil_*`

| Metric | Type | Notes |
|:---|:---|:---|
| `request_seconds{verb,result}` | histogram | Per-verb server-side latency. anvil is a single Deployment with a strict request/response protocol, so a slow verb serialises every login that needs it. |
| `sessions` | gauge | Tracked login sessions. |
| `sessions_reaped_total` | counter | TTL evictions. A rising rate means sessions are losing their heartbeat, and each reap makes the next `HEARTBEAT` answer `reason=unknown` to a session that is in fact alive. |
| `penalty_lookups_total{result}` | counter | `hit` (penalty in force) \| `miss` \| `expired`. |
| `connections` / `connections_total` | gauge / counter | Saturation signal for a single-replica service. |

### `yarilo_director_lookup_seconds`

Server-side `LOOKUP` latency by outcome: `sticky`, `assigned`, `killing`,
`no_backends`, `bad_request`. `killing` and `no_backends` both make a login
proxy retry, but only one of them is a healthy state — a single latency series
cannot express that.

### Notes

Collection is unconditional; there is no enable/disable knob. The cost is a
handful of counters and histograms per request, and a metric that has to be
switched on is never on when the incident starts.

---

## Log verbosity

`logLevel` sets the level for the whole installation (default `info`); it reaches
every container as `LOG_LEVEL`.

`logLevelOverrides` raises or lowers it for individual components, keyed by the
name the container reports in `YARILO_COMPONENT`:

```yaml
logLevel: info
logLevelOverrides:
  yarilo-auth: debug
  yarilo-imap-login: debug
```

Keeping the installation at `info` and raising one component on demand is the
intended pattern. Cluster-wide `debug` under load produces enough volume that
kubelet rotates the container log away — during the #878 investigation the
backend log for the exact window being analysed was already gone by the time it
was read.

`LOG_LEVEL` remains the startup default, so nothing about an existing
deployment changes.

### Changing the level at runtime (#889)

The level lives in a `slog.LevelVar`, so it can be changed **without restarting
the pod** — which matters because a restart destroys the state being
investigated:

```
GET  /debug/loglevel                                → {"level":"info"}
POST /debug/loglevel {"level":"debug"}              → until further notice
POST /debug/loglevel {"level":"debug","ttl":"30s"}  → reverts automatically
```

Prefer the TTL form. A bounded raise cannot be forgotten in the on position,
which is the usual way a debug switch ends up rotating away the log it was
supposed to capture.

The endpoint is served on the telemetry listener (`:8080`) only, which is not
exposed to mail clients. It must never be published on a client-facing Service.

Example — raise `yarilo-auth` to debug for half a minute:

```
kubectl -n yarilo-sb exec deploy/yarilo-auth -- \
  wget -qO- --post-data '{"level":"debug","ttl":"30s"}' localhost:8080/debug/loglevel
```

### `yarilo_log_level`

The active level is also published as a gauge, so a change can be confirmed from
the same place every other metric is read. The value is slog's numeric level and
the label carries the name; exactly one series exists at a time.

```
yarilo_log_level{level="debug"} -4
```

---

## Transient-failure retries (#896)

Three login-path failures are temporary by definition and are now retried before
the client is told anything: `yarilo-auth` reporting temp-fail, the first dial to
auth, and bringing up the backend session (dial + preamble + greeting, retried as
one unit because a failed greeting leaves the connection unusable).

Budget: `login.transientRetries`, default **3**, `150ms` between attempts. A
negative value restores fail-on-first-error.

### `yarilo_login_transient_retries_total{protocol,stage}`
### `yarilo_login_transient_exhausted_total{protocol,stage}`

Stages: `auth_dial`, `auth`, `backend_session`.

Read them as a pair — that is the whole point:

| retries | exhausted | meaning |
|---|---|---|
| flat | flat | nothing transient is happening |
| rising | flat | **the budget is absorbing blips** — a dependency is flapping but no client saw it |
| rising | rising | the outage outlasts the budget; the fix is upstream, not a bigger budget |

```promql
# share of transient failures that still reached a client
sum(rate(yarilo_login_transient_exhausted_total[5m]))
  / sum(rate(yarilo_login_transient_retries_total[5m]))
```

A raise in `transientRetries` is not a fix for the third row: it only lengthens
how long a login waits before failing anyway.

Not covered: `preamble_error`. There the client connection is already broken or
sent something unparseable, so there is nothing to retry against.

---

## Telemetry is one implementation

Every component serves `/healthz`, `/readyz`, `/metrics` and `/debug/loglevel`
from `internal/telemetry`. Before unification each binary built its own mux,
which is how `/debug/loglevel` came to exist in two components out of fourteen
while the other twelve answered 404, and how `/readyz` came to be an
unconditional 200 in eleven of them.

### Wiring a component

```go
tel := telemetry.NewWithOptions(telemetry.Options{
    Addr:     telemetry.Addr(cfg.Telemetry.Listen),
    Registry: reg,                       // nil = the default registry
    Checks: []telemetry.Check{           // readiness conditions
        telemetry.TCPCheck("auth", authAddr, authTLS),
        telemetry.FuncCheck("backend", backendReady),
    },
    Lifecycle: true,                     // also require SetReady(true)
})
go tel.ListenAndServe(ctx)
```

`TCPCheck` with a TLS config completes the handshake too, so a component whose
certificate is wrong reports not-ready rather than "port accepts".

An empty `Checks` list means the process being up **is** the condition — a
legitimate answer, but state it deliberately.

### `/readyz`

Answers JSON naming every condition, so a failing probe says which dependency is
missing:

```json
{"ready":false,"checks":[{"name":"auth","ok":false,"error":"connection refused"},
                         {"name":"redis","ok":true}]}
```

Checks run concurrently under an 800ms deadline, so readiness latency is the
slowest dependency rather than their sum, and a hung dependency surfaces as
not-ready instead of as a probe timeout.

### `yarilo_readiness_check{check}`

Each condition is also published as a gauge (1 = passing), so a not-ready pod can
be diagnosed from metrics without shelling into it.

### What `/readyz` means

**Fully initialised**: storage opened, dependencies wired, and **every configured
port bound and accepting**. Not "some checks passed on this request".

The ordering matters and used to be wrong. Readiness was reported immediately
after starting the telemetry server, while the protocol listeners were bound
inside goroutines that had not necessarily run yet:

```go
s.telem.SetReady(true)                  // pod announced as ready
go func() { s.imap.ListenAndServeTLS() }()   // port bound only now
```

Kubernetes adds a pod to the Service endpoints the moment it goes ready, so a
client arriving in that window got `connection refused` — most likely during a
rollout, which is exactly when traffic shifts to a freshly-ready pod.

Every `Run*` now binds first and reports ready afterwards, so a bind failure is a
startup error instead of a log line from a goroutine in a pod that already claimed
to be serving.

Readiness stays a **lifecycle flag**, not a set of per-request probes: a
dependency that is unreachable at startup is caught by `backend.New` failing, and
one that fails later belongs in the client-facing error path.
