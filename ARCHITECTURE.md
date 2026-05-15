# yarilo — Architecture

This document is the **authoritative reference** for yarilo's architecture.
All development decisions must be consistent with what is written here.
If something in the code contradicts this document — the code is wrong.

---

## Core principles

- **Security** — privilege separation: each process runs with minimum required UID/permissions.
- **Process lightness** — each process does one thing, minimal memory footprint.
- **Fault tolerance** — crash of one process does not affect others; master restarts failed children.
- **Scalability** — stateless components scale horizontally via HPA; stateful components use director affinity.
- **Isolation** — one session = one process; a compromised session cannot access other users' mail.

---

## Process model

yarilo is a **multi-binary, multi-process** system. Each component is a separate compiled binary.
There is no single monolithic binary. There is no `mode:` flag dispatch in one binary.

### Binary layout

```
/usr/lib/yarilo/
  yarilo-master
  yarilo-log
  yarilo-config
  yarilo-anvil
  yarilo-auth
  yarilo-auth-worker
  yarilo-imap-login
  yarilo-imap
  yarilo-pop3-login
  yarilo-pop3
  yarilo-submission-login
  yarilo-submission
  yarilo-lmtp
  yarilo-director
  yarilo-health
  yarilo-ipc
```

### Source layout

```
app/
  yarilo-master/main.go
  yarilo-log/main.go
  yarilo-config/main.go
  yarilo-anvil/main.go
  yarilo-auth/main.go
  yarilo-auth-worker/main.go
  yarilo-imap-login/main.go
  yarilo-imap/main.go
  yarilo-pop3-login/main.go
  yarilo-pop3/main.go
  yarilo-submission-login/main.go
  yarilo-submission/main.go
  yarilo-lmtp/main.go
  yarilo-director/main.go
  yarilo-health/main.go
  yarilo-ipc/main.go
internal/
  master/          — supervisor: exec, monitor, restart, fd-passing
  login/imap/      — imap-login: TLS accept + SASL + session proxy goroutine
  login/pop3/
  login/submission/
  imap/            — IMAP session (plain Unix socket, no TLS)
  pop3/
  submission/
  lmtp/
  auth/            — passdb/userdb chain
  anvil/           — connection accounting
  director/        — consistent hash ring, user→backend routing
  health/          — backend health probes
  ipc/             — inter-process command routing
pkg/
  mailbox/         — MailboxBackend + IndexBackend interfaces
  config/          — YAML config via koanf
```

---

## Process hierarchy and UIDs

| Process | UID | Role |
|:---|:---|:---|
| `yarilo-master` | root | supervisor: exec/monitor/restart all children |
| `yarilo-log` | root | centralized log pipe sink |
| `yarilo-config` | root | config serving via Unix socket |
| `yarilo-anvil` | yarilo | connection accounting per user+IP |
| `yarilo-auth` | yarilo | passdb/userdb chain |
| `yarilo-auth-worker` | yarilo | blocking SQL/LDAP passdb backends |
| `yarilo-imap-login` | dovenull | TLS accept + SASL; TLS proxy for session lifetime |
| `yarilo-pop3-login` | dovenull | TLS accept + SASL; TLS proxy for session lifetime |
| `yarilo-submission-login` | dovenull | TLS accept + SASL; TLS proxy for session lifetime |
| `yarilo-imap` | mail uid | IMAP session — plain Unix socket, maildir access only |
| `yarilo-pop3` | mail uid | POP3 session |
| `yarilo-submission` | mail uid | Submission session |
| `yarilo-lmtp` | root | LMTP delivery; setuid per recipient during delivery |
| `yarilo-director` | yarilo | consistent hash ring, user→backend affinity |
| `yarilo-health` | yarilo | backend health probes, updates director ring |
| `yarilo-ipc` | yarilo | inter-process command bus (kick-user, admin) |

---

## Startup sequence

```
yarilo-master (root)
  │
  ├─► spawn yarilo-log      (pipe, root)       ← first; all subsequent log through it
  ├─► spawn yarilo-config   (socket, root)
  ├─► spawn yarilo-anvil    (socket, yarilo)
  ├─► spawn yarilo-auth     (socket, yarilo)
  │     └─► spawn yarilo-auth-worker on demand
  ├─► spawn yarilo-director (socket, yarilo)
  ├─► spawn yarilo-health   (socket, yarilo)
  ├─► spawn yarilo-ipc      (socket, yarilo)
  │
  ├─► bind ports 993, 143, 995, 110, 465, 587, 24  ← root only
  │
  ├─► spawn yarilo-imap-login       ×N  (pass listening fd 993/143)
  ├─► spawn yarilo-pop3-login       ×N  (pass listening fd 995/110)
  ├─► spawn yarilo-submission-login ×N  (pass listening fd 465/587)
  └─► spawn yarilo-lmtp             ×M  (pass listening fd 24)
```

---

## Connection lifecycle

### IMAP (port 993)

```
client ──TLS:993──► yarilo-imap-login (dovenull)
                        │ TLS handshake
                        │ SASL auth ──► yarilo-auth (Unix socket)
                        │ conn check ──► yarilo-anvil (Unix socket)
                        │ FAIL → close
                        │ OK:
                        │   create Unix socket pair
                        │   send (conn_fd + session JSON) → yarilo-master
                        │
                    yarilo-master (root)
                        │ exec yarilo-imap
                        │   SysProcAttr.Credential{Uid: mail_uid, Gid: mail_gid}
                        │   ExtraFiles: [unix_socket_end]
                        │
                    yarilo-imap (mail uid)
                        │ plain IMAP over Unix socket
                        │ maildir access (own UID only)
                        │
                    yarilo-imap-login (dovenull) — still running as TLS proxy
                        read TLS conn → write Unix socket → yarilo-imap
                        read Unix socket → write TLS conn → client
                        exit when yarilo-imap exits
```

### LMTP (port 24)

```
MTA ──TCP:24──► yarilo-lmtp (root)
                    auth lookup ──► yarilo-auth
                    per recipient: setuid(recipient_uid)
                    write to maildir
                    setuid(root)
```

---

## Inter-process communication

| From | To | Mechanism | Protocol |
|:---|:---|:---|:---|
| all children | `yarilo-log` | pipe (fd inherited at exec) | JSON lines |
| `*-login` | `yarilo-auth` | Unix socket | TAB-delimited (INTERNALS.md §3) |
| `*-login` | `yarilo-anvil` | Unix socket | TAB-delimited |
| `*-login` | `yarilo-director` | Unix socket | TAB-delimited (INTERNALS.md §2) |
| `*-login` | `yarilo-master` | Unix socket + SCM_RIGHTS | fd + JSON session state |
| all children | `yarilo-master` | status pipe | binary status frames |
| `yarilo-health` | `yarilo-director` | Unix socket | TAB-delimited |
| admin | `yarilo-ipc` | Unix socket | TAB-delimited |

### Socket paths

```
/run/yarilo/master.sock
/run/yarilo/auth.sock
/run/yarilo/anvil.sock
/run/yarilo/director.sock
/run/yarilo/health.sock
/run/yarilo/ipc.sock
/run/yarilo/config.sock
```

---

## Go implementation rules

### Process spawning — exec.Command only

`fork()` after goroutine runtime start is undefined behavior in Go.
**Always use `exec.Command`**, never `syscall.Fork()`.

```go
cmd := exec.Command("/usr/lib/yarilo/yarilo-imap-login")
cmd.SysProcAttr = &syscall.SysProcAttr{
    Credential: &syscall.Credential{Uid: dovenullUID, Gid: dovenullGID},
    Setsid:     true,
}
cmd.ExtraFiles = []*os.File{listeningSocketFile}
```

### Privilege drop — SysProcAttr.Credential only

`syscall.Setuid()` does not work in multi-threaded Go (affects only one OS thread).
Privilege drop must happen at process start via `exec.Cmd.SysProcAttr.Credential`.
The kernel applies setuid/setgid before the Go runtime initializes in the child.

### TLS — login process is TLS terminator for session lifetime

Go's `crypto/tls.Conn` cannot be serialized across process boundary.
The login process (`yarilo-imap-login`) retains the TLS connection for the entire session,
acting as a transparent proxy between the TLS conn and the session process (plain Unix socket).
Session processes (`yarilo-imap`) receive a plain Unix socket — they have zero TLS knowledge.

### fd-passing — SCM_RIGHTS via net.UnixConn

```go
// send fd:
rights := syscall.UnixRights(int(f.Fd()))
conn.WriteMsgUnix([]byte(sessionJSON), rights, nil)

// receive fd:
conn.ReadMsgUnix(buf, oob, nil)
fds, _ := syscall.ParseUnixRights(&msg)
```

Master binds ports (root), passes listening fd to login processes via `cmd.ExtraFiles`.
Login processes pass authenticated conn fd to master via SCM_RIGHTS after auth success.

### Process supervision

Each child process is supervised by a dedicated goroutine calling `cmd.Wait()`.
On unexpected exit: respawn with exponential backoff (min 2s, max 60s).
If a process exits within 5s of start more than 5 times: throttle and alert via log.

---

## Kubernetes deployment

yarilo runs as a **multi-process Pod** in Kubernetes.
All processes run inside a single container. Master is supervised by `tini` (PID 1).
Unix sockets live in an `emptyDir` volume at `/run/yarilo/`.

### k8s objects per component

| Deployment | Service type | Replicas | Notes |
|:---|:---|:---|:---|
| `yarilo-auth` | ClusterIP | 2+ | stateless, HPA |
| `yarilo-anvil` | ClusterIP | 1 | shared conn state, single instance |
| `yarilo-director` | ClusterIP | 2–3 | ring state, leader election |
| `yarilo-imap-login` | LoadBalancer :993/:143 | 2+ | stateless, HPA |
| `yarilo-pop3-login` | LoadBalancer :995/:110 | 2+ | stateless, HPA |
| `yarilo-submission-login` | LoadBalancer :465/:587 | 2+ | stateless, HPA |
| `yarilo-imap` | ClusterIP (internal) | 2+ | NFS/CephFS RWX, director affinity |
| `yarilo-pop3` | ClusterIP (internal) | 2+ | NFS/CephFS RWX, director affinity |
| `yarilo-submission` | ClusterIP (internal) | 2+ | stateless relay, HPA |
| `yarilo-lmtp` | ClusterIP (internal MTA) | 2+ | NFS/CephFS RWX |

### k8s replaces

| Dovecot process | k8s equivalent |
|:---|:---|
| `yarilo-log` | stdout → k8s log collection (fluentd/loki) |
| `yarilo-config` | ConfigMap mounted as file |
| `yarilo-stats` | `/metrics` per Pod, Prometheus ServiceMonitor |

### Storage

Maildir requires shared filesystem for multi-replica `yarilo-imap`/`yarilo-pop3`/`yarilo-lmtp`:
- **CephFS** (preferred for production) — distributed, no SPOF, native k8s CSI via `rook-ceph`
- **NFS** — simpler setup, single NFS server

`yarilo-director` ensures user→pod affinity (same user always routed to same pod under normal operation).
On pod failure, director reroutes to another pod — NFS/CephFS RWX allows any pod to access any maildir.

```yaml
persistence:
  accessMode: ReadWriteMany
  storageClass: cephfs   # or nfs
```

---

## Security model

| Threat | Mitigation |
|:---|:---|
| Exploit in TLS/SASL handling | `yarilo-imap-login` runs as `dovenull` — no maildir access, no auth secrets |
| Exploit in IMAP session | `yarilo-imap` runs as mail uid — access only to own maildir |
| Cross-user maildir access | UID isolation: each session process has exactly one user's UID |
| Auth bypass | `yarilo-auth` isolated process — imap/pop3 cannot call passdb directly |
| Connection flooding | `yarilo-anvil` enforces `max_userip_connections` globally across all login replicas |
| Backend failure | `yarilo-health` detects, `yarilo-director` removes from ring, reroutes in-flight |
