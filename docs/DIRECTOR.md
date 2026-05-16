# Director configuration

`yarilo-director` is the consistent-hash routing front-end for the yarilo mail cluster.
It accepts IMAP/POP3/LMTP connections from mail clients, extracts the username from the
protocol preamble, maps each username to a specific backend pod via a consistent-hash ring,
and proxies the session directly to that pod IP — bypassing kube-proxy so the same user
always lands on the same pod (and the same mailbox).

---

## How it works

```
mail client
    │
    │  TLS (IMAPS/POP3S) or plain TCP (IMAP/POP3/LMTP)
    ▼
yarilo-director  (LoadBalancer Service, ports 993/143/995/110/24)
    │
    │  1. Optional: read HAProxy PROXY header → real client IP
    │  2. TLS-terminate (IMAPS / POP3S)
    │  3. Extract username from protocol preamble
    │        IMAP  → LOGIN / AUTHENTICATE PLAIN
    │        POP3  → USER / PASS
    │        LMTP  → LHLO / MAIL FROM / RCPT TO (first recipient = routing key)
    │  4. Consistent-hash ring lookup → backend pod IP
    │  5. Dial backend pod directly (pod IP, not service VIP)
    │  6. Optional: send XCLIENT ADDR=<real-ip> to backend
    │  7. Replay auth command to backend
    │  8. Bidirectional TCP proxy for the rest of the session
    │
    ▼
yarilo-imap / yarilo-pop3 / yarilo-lmtp  (headless Service, pod IP)
```

The director speaks just enough of each protocol to extract the username, then becomes
a transparent TCP proxy. The backend pod sees the original client commands — it handles
the full session including authentication against yarilo-auth.

---

## `director_service`

Ring and lifecycle settings for the director process.

| Key | Default | Description |
|:---|:---|:---|
| `listen` | `":9102"` | Address for the director-to-director ring protocol (internal, not mail ports). |
| `user_expire` | `900` | Seconds before a user→backend mapping expires from the in-memory directory. Active sessions reset the TTL on every lookup. |
| `ping_interval` | `30` | Seconds between keepalive pings to peer directors (ring health). |
| `ping_timeout` | `10` | Seconds to wait for a PONG before closing the peer connection. |
| `shutdown.session_grace_period` | `30` | Seconds to wait after SIGTERM before force-closing sessions. |
| `shutdown.kill_timeout` | `5` | Seconds after grace period before hard exit. |
| `peers` | `[]` | List of peer director addresses (`"host:port"`) for ring sync. Required when `replicas > 1`. Each director must list all other replicas. |

---

## `director_service.mail_servers`

Static backend list loaded at startup. Each entry resolves to one or more pod IPs via DNS
(headless k8s services return one A-record per pod). All resolved IPs are added to the
consistent-hash ring.

| Key | Description |
|:---|:---|
| `host` | Hostname of the headless k8s Service, e.g. `yarilo-imap.yarilo-backend.svc.cluster.local`. |
| `port` | Backend container port the director dials (must match the pod's listen port). |
| `tag` | Optional pool label. Empty string = default pool. Used when one director serves multiple backend groups. |

```yaml
director_service:
  listen: ":9102"
  user_expire: 900
  ping_interval: 30
  ping_timeout: 10
  mail_servers:
    - host: yarilo-imap.yarilo-backend.svc.cluster.local
      port: 993
      tag: ""
    - host: yarilo-pop3.yarilo-backend.svc.cluster.local
      port: 110
      tag: ""
    - host: yarilo-lmtp.yarilo-backend.svc.cluster.local
      port: 24
      tag: ""
```

---

## `services` (director listeners)

The director binds the same mail-protocol ports as a regular yarilo node. The `services`
block in the director config controls which ports are active. Fields are identical to a
regular yarilo node — see [SERVICES.md](SERVICES.md).

Typical director setup exposes only the ports clients connect to:

```yaml
services:
  imaps:
    enabled: true
    port: 993
    ssl_mode: ssl
    haproxy_protocol: true   # if an upstream LB forwards PROXY headers
  imap:
    enabled: false           # disable if not needed
  pop3s:
    enabled: false
  pop3:
    enabled: false
  lmtp:
    enabled: true
    port: 24
    ssl_mode: "no"
    xclient_protocol: true   # director will forward real IP to lmtp backend
```

---

## HAProxy PROXY protocol

When a load balancer (HAProxy, nginx, AWS NLB) sits in front of the director, it can
forward the original client IP in a `PROXY` header prepended to the TCP stream:

```
PROXY TCP4 203.0.113.42 10.0.0.1 41234 993\r\n
<TLS ClientHello...>
```

Enable in config:

```yaml
services:
  imaps:
    enabled: true
    haproxy_protocol: true   # per-listener flag

general:
  haproxy:
    enabled: true            # global flag (controls all haproxy_protocol: true listeners)
    trustedNets:
      - "10.0.0.0/8"        # only accept PROXY headers from these source IPs
    timeout: 3               # seconds to wait for the PROXY header
```

The director reads the PROXY header before TLS handshake. After the header is parsed,
`conn.RemoteAddr()` returns the real client IP for the rest of the connection — this IP
is used in logs and forwarded to the backend via XCLIENT (if enabled).

Connections from IPs not in `trustedNets` have the PROXY header silently ignored; the
raw TCP address is used instead.

---

## XCLIENT forwarding

After connecting to a backend pod, the director can forward the real client IP via the
`XCLIENT` command so the backend session sees the original client instead of the director's
pod IP. This is important for per-IP connection limits, logging, and audit trails.

Enable in config:

```yaml
services:
  imaps:
    xclient_protocol: true   # director will send XCLIENT to imaps backend
  lmtp:
    xclient_protocol: true

general:
  xclient:
    trustedNets:
      - "10.0.0.0/8"        # written into the backend config; backend trusts these IPs
```

Wire format per protocol:

| Protocol | Director sends | Backend responds |
|:---|:---|:---|
| IMAP / IMAPS | `XCONN XCLIENT ADDR=<ip>\r\n` | `XCONN OK XCLIENT\r\n` |
| POP3 / POP3S | `XCLIENT ADDR=<ip>\r\n` | `+OK XCLIENT accepted\r\n` |
| LMTP | `XCLIENT ADDR=<ip>\r\n` | `220 2.0.0 OK\r\n` |

XCLIENT is sent immediately after the backend greeting is consumed, before auth replay.
The backend must list the director's pod CIDR in its own `xclient.trustedNets` — the
`general.xclient.trustedNets` value in the helm chart is written to the backend config
for this purpose.

---

## mTLS (internal connections)

All director-to-backend and director-to-director connections use mTLS when
`internal_tls.enabled: true`. Certificate and CA are mounted from the same k8s Secret
used by all internal components.

```yaml
internal_tls:
  enabled: true
  cert: /etc/yarilo/internal-tls/tls.crt
  key:  /etc/yarilo/internal-tls/tls.key
  ca:   /etc/yarilo/internal-tls/ca.crt
```

When `enabled: false` (default), all internal connections are plain TCP. Acceptable when
a service mesh (Istio, Linkerd) handles transport security.

---

## Helm values

All director settings live under `components.director` in `helm/values.yaml`.

| Helm value | Config key | Description |
|:---|:---|:---|
| `components.director.directorPort` | `director_service.listen` | Ring protocol port (`:9102`). |
| `components.director.userExpire` | `director_service.user_expire` | User→backend TTL (seconds). |
| `components.director.pingInterval` | `director_service.ping_interval` | Peer keepalive interval (seconds). |
| `components.director.pingTimeout` | `director_service.ping_timeout` | Peer keepalive timeout (seconds). |
| `components.director.backends[]` | `director_service.mail_servers[]` | Static backend list. |
| `components.director.internalTLS.enabled` | `internal_tls.enabled` | Enable mTLS on internal connections. |
| `components.director.tls.secretName` | — | k8s Secret for the external (client-facing) TLS cert. |
| `components.director.listeners.*` | `services.*` | Per-protocol listener ports and enable flags. |
| `general.haproxy.enabled` | `services.*.haproxy_protocol` | Enable HAProxy PROXY protocol on all listeners. |
| `general.haproxy.trustedNets` | `general.haproxy.trusted_nets` | Source IPs trusted to send PROXY headers. |
| `general.haproxy.timeout` | `general.haproxy.timeout` | Seconds to wait for PROXY header. |
| `general.xclient.enabled` | `services.*.xclient_protocol` | Enable XCLIENT forwarding on all listeners. |
| `general.xclient.trustedNets` | `general.xclient.trusted_nets` | CIDRs written to backend config as trusted XCLIENT sources. |

### Minimal helm values (single-node IMAPS + LMTP)

```yaml
components:
  director:
    enabled: true
    listeners:
      imaps:
        enabled: true
        port: 993
        containerPort: 10993
      lmtp:
        enabled: true
        port: 24
        containerPort: 10024
    backends:
      - host: yarilo-imap.yarilo-backend.svc.cluster.local
        port: 993
        tag: ""
      - host: yarilo-lmtp.yarilo-backend.svc.cluster.local
        port: 24
        tag: ""
    tls:
      secretName: yarilo-tls

general:
  haproxy:
    enabled: false
  xclient:
    enabled: false
```
