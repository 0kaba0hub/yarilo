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

The director exposes backend health via `/metrics`:

```
# Ring membership and status (1 = present; status = up | flush)
yarilo_director_backend_info{ip="10.0.0.1", port="993", tag="ssd", status="up"} 1

# Approximate session count from userDir TTL window
yarilo_director_backend_sessions{ip="10.0.0.1", port="993", tag="ssd"} 42
```

`backend_sessions` counts non-expired user→backend entries in the director's userDir
(TTL = `user_expire`, default 900 s). It is an approximation — exact session counts
require SESSION-OPEN/SESSION-CLOSE tracking (planned).

Alert example (Prometheus):
```promql
yarilo_director_backend_info{status!="up"} == 1
```
