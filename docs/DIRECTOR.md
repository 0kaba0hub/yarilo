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

## Session routing & sticky assignments

Every login proxy (imap/pop3/submission/managesieve/lmtp) routes sessions one of two ways: `backend_addr` (standalone, a fixed backend) or `director_addr` (director mode, per-session `LOOKUP` via yarilo-director) — at least one is required, and `backend_addr` wins when both are set (#735, unified across all five components including lmtp-login in #741). `backend_port` overrides the port a director `LOOKUP` returns when it differs from the backend's protocol-specific containerPort.

**Migration note (#741):** lmtp-login previously had the *opposite* precedence — `director_addr` won when both were set. If your `lmtpLogin` Helm values set both `backend_addr` and `director_addr`, the login pod now logs a startup warning and silently switches from director routing to the static backend. Remove `backend_addr` from `lmtpLogin` values to keep director routing.

`director_service.username_hash_lowercase` (default `true`, Helm: `components.director.username_hash_lowercase`) lowercases usernames before they're hashed for ring routing or used as keys for sticky assignments and admin (`USER-MOVE`) overrides (#738) — without it, two spellings of the same account (`User@d.test` / `user@d.test`) can hash to different values and land on different backends, defeating sticky routing. Migration note: enabling this on an already-running cluster changes hashes for mixed-case usernames — their existing sticky entries just expire naturally via `user_expire`, no special migration step is needed.

`director_service.username_hash` (default `""`, Helm: `components.director.username_hash`) is the username→hash-key template (#850), which mirrors the reference's `director_username_hash` expression so a `dovecot.conf` value migrates **verbatim** — set `username_hash: "%Lu"` for `director_username_hash = %Lu`. Supported variables: `%u` (whole username), `%n` (local part, before the first `@`), `%d` (domain, after the first `@`), each with an optional `%L` lowercase modifier, plus `%%` for a literal percent. This is a real routing lever, not just config parity: `%Ld` hashes on the **domain only** so a whole domain (and its shared mailboxes/ACLs) lands on one backend, and `%Ln` hashes on the local part only for alias-domain installs. A domain-less username follows the reference semantics — `%n` is the whole username, `%d` is empty (so a `%d` template routes every domain-less account to one backend). When `username_hash` is set it — not `username_hash_lowercase` — governs case-folding (the ingress no longer pre-lowercases, so `%u` is truly case-sensitive), and the `USER-KICKED` payload keeps the session's original-case username so login-side kick matching (#701) still lands. An empty value derives the template from `username_hash_lowercase` (`%Lu` / `%u`) for byte-identical back-compat with pre-#850 clusters. An invalid template aborts director startup. (yarilo's uint32 fold is little-endian since #738 — deliberately not byte-compatible with the reference's ring, a scenario our architecture never produces; we borrow the routing semantics, not the byte layout.)

**`%d` domain-hash — read before you use it.** The hash is applied **within a tag**, not globally: every `LOOKUP` carries a per-user tag (#737, tag = NFS shard) and the ring is tag-scoped (`LookupBackendByTag`), exactly like the reference's per-tag `mail_host_get_by_hash`. So a `%Ld` template routes a domain to **one backend per tag** — if the domain's mailboxes are spread across tags (the tag is assigned per user by userdb, independent of the hash), those users land on different tag rings and different backends; `%d` does **not** collapse a multi-tag domain onto a single host. The failure mode to avoid is putting one large domain entirely in **one** tag: there `%Ld` pins the whole domain to a single backend with **no load rebalancing** (yarilo, like the reference, distributes only by consistent hashing + vhost capacity weighting; it never auto-spreads a hot key). The cure is spreading the domain across tags via userdb, not changing the hash. Use `%Ld` only when a domain's shared mailboxes/ACLs genuinely must be storage-local within a tag.

**Backend evacuation — graceful vs force (#849).** `yarctl director backends flush <ip|all>` drains a backend. By default the drain is **graceful and throttled**: the host is taken out of the ring and its users are migrated in a self-clocked window of at most `director_service.max_parallel_moves` (default `5`, Helm: `components.director.max_parallel_moves`) confirmed-kills — each user's old sessions must confirm gone before the next user is pulled in, so a planned drain (rolling upgrade, maintenance) spreads the re-login across the surviving pods instead of stampeding them all at once. `--force` restores the pre-#849 behaviour (kick every session immediately); `--max-parallel N` overrides the window for one run. Re-login is deterministic without a proactive pin move: the evacuating host is already excluded from the ring, so a kicked user's re-`LOOKUP` rehashes to the same surviving backend it would have been moved to, and the confirmed-kill hold (#847) makes that re-`LOOKUP` wait until the old session is gone (no split-writer window). The drain is orchestrated by the single director that receives the request; if that director is lost mid-drain the operator re-runs `flush` (drain-state is not replicated across directors — matching the reference's `self_host` origination). Matches the reference's `doveadm director flush -F` / `--max-parallel` and `director_max_parallel_moves`.

**Per-user flush hook (#848).** `director_service.flush_program` (default `""` = disabled, Helm: `components.director.flush_program`) is an optional external executable run once per user **after** a deliberate relocation — an admin `USER-MOVE` or a graceful evacuation — has been confirmed ring-wide, i.e. after that user's old sessions are gone. Operators hook mailbox-cache flush, external session cleanup, metrics, etc. It is called as `flush_program FLUSH <username> <username_hash> <old_backend> <new_backend>` (`new_backend` is empty when the user was kicked with no surviving backend to land on). It is **best-effort and asynchronous** with a bounded 10s timeout: a slow or failing hook is logged and never blocks the ring/`LOOKUP` path or fails the move (the routing change already committed). Only the director that **originated** the move runs it — mass/reactive paths (`backend-down` auto-kick, `--force` flush) deliberately do not trigger it, and a move that creates a fresh pin with no prior host is skipped. This is yarilo's analog of the reference's `director_flush_socket` (which runs its hook at the same point — the ring-wide `USER-KILLED-EVERYWHERE`); the program runs in the director pod's context, `exec.Command` only (no fork). A unix-socket hook variant is a possible future follow-up. An **offline** user — one with no active sessions when the move starts — confirms after `user_kill_confirm_grace` (~1s) instead of waiting out `user_kill_timeout`, so the hook fires promptly for the common admin case of moving a user who isn't currently connected (#870); a user with live sessions still confirms only once those sessions have drained.

### Tag sharding models

Every director `LOOKUP` carries a mandatory tag field — there is no full-ring mode (#737): `""` selects the untagged backend pool, not "any tag." Two sharding models are supported:

- **Static (dedicated login fleet per tag pool).** Each login component's `director_tag` (Helm: `components.<login>.director_tag`, e.g. `components.imapLogin.director_tag`) restricts that component's lookups to one tag-pool — set this when running a dedicated login Deployment per tag pool, per `docs/DEPLOYMENT.md`'s tag-based sharding model. In a deployment with no tags configured at all, every backend is untagged, so the default `director_tag: ""` behaves exactly like the old (buggy) full-ring lookup — untagged/standalone deployments see no behavior change.
- **Shared (per-user tag from passdb/userdb, #746).** One login fleet serving users of every tag-pool: a `director_tag` extra field on the passdb or userdb response (SQL: an ordinary column in `password_query`/`user_query`, same generic column-forwarding as `allow_nets` — no driver code change needed) picks the tag for that one user's `LOOKUP`, overriding the component's static `director_tag`. IMAP/POP3/Submission/ManageSieve pick it up from the AUTH response; LMTP resolves it with a per-recipient userdb lookup before the director `LOOKUP`. A user with no `director_tag` field falls back to the component's static value.

## Ring formation & design history

Director replicas self-organize into a ring at runtime (#750 phase 1 — replaces the earlier static full-mesh `peers` list, #700): members are ordered by `(ip, port)` and each dials only its right neighbor, never a full mesh. Every member count is a fully valid, service-serving state (never refuses service) — a lone director is an ordinary N=1 ring, no peer machinery runs at all. `components.director.peers` is now a **seed list**: each entry is tried in turn for a one-time `DIRECTOR-JOIN`, after which membership maintains itself via propagation. Left empty (the default), at `replicas > 1` the seed auto-derives to the headless `<release>-director-ring:9102` Service (#751/#764) — never the ClusterIP `<release>-director` Service — because only the headless name resolves directly to every ready pod's IP, which is what the DNS fan-out needs to poll each peer; the ClusterIP resolves to a single virtual IP that load-balances dials randomly and reintroduces the formation partition. An explicit list overrides this (non-k8s / manual seeding). `components.director.ring_secret` (auto-generated into Secret `<release>-director-ring-secret`, mirroring the API token) authenticates joins via HMAC-SHA256 — leaving it unset rejects every join attempt outright, so that replica can only ever run standalone. `components.director.min_members` (default 3) is an install-time warning only, no runtime effect. Phase 1 covers ring topology, membership propagation, and the HMAC join core; dial-back verification + CIDR filtering (phase 2), full user/backend state snapshot on connect (phase 3), and members_hash anti-entropy (phase 4) are tracked separately.

**#754 (found in the first live 3-replica sandbox test)** fixed a phase-1 regression where killing one ring member left the membership set permanently corrupted: a dead member's tombstone wasn't propagated (an ordering bug meant `DIRECTOR-REMOVE` was announced before the outgoing connection needed to send it existed) and, separately, a plain member-list union on every reconnect could silently resurrect a member some other node had already correctly evicted. Membership now carries a proper tombstone set, exchanged alongside the member list on every ring connection; `Member` ordering also now sorts by parsed IP octets instead of string comparison (`"10.0.0.17" < "10.0.0.6"` as strings, backwards from the real address).

**#755** fixed `yarctl director ...` returning 403 from every pod, including the director pod's own shell — the director admin API token and URL were never plumbed to `yarilo-backend-api` (the standard admin plane) or reliably available for local use. Both are now wired via a shared Helm env-injection helper, and the empty-by-default `api.allowed_nets` (see #759) means the bearer token is the sole gate rather than a cluster-specific CIDR. The `smoketest` binary now carries a `-director-api <url>` check (bearer token from `-director-api-token` or `DIRECTOR_API_TOKEN`/`YARILO_ADMIN_TOKEN`) that fails loudly on a 403/401 and asserts a member list on 200 — run it in-cluster (a Job or `kubectl exec` on `yarilo-backend-api`, where the token env is already injected) since the director API is a ClusterIP; `smoke.yml` gained an optional `director_api` input for the same check from an in-cluster runner.

**#758** replaced the ring's single-edge event-forward path (one connection picked at accept time — `dialConn`, or a `passiveConn` reserved for the N=2 tie-break's passive member) with a broadcast to every currently live ring connection except whichever one an event just arrived on, matching the reference's `director_update_send` skip-arrival model instead of a fixed per-connection role that could go stale mid-connection across an N=3→N=2 shrink.

**#759 (found in a live 3-pod simultaneous-start sandbox test)** fixed a load-balanced ClusterIP seed routing a pod's own `DIRECTOR-JOIN` dial back to itself: this looked like an ordinary, immediate join success (a self-join is a harmless no-op), so `joinLoop` stopped retrying the seed forever, leaving the pod stuck as a permanently isolated N=1 that never discovered any real peer. `handleJoin` now rejects a self-dial explicitly, so the existing generic retry keeps dialing the seed until kube-proxy routes it elsewhere. Also dropped `components.director.api.allowed_nets`'s hardcoded kubeadm-shaped default (`10.96.0.0/12` + `10.244.0.0/16`) — wrong for any cluster with different service/pod CIDRs, silently 403ing every request including well-authenticated ones — in favor of an empty default (token-only auth; CIDR filtering is opt-in defense-in-depth once an operator knows their real cluster CIDRs).

**#759 follow-up (live re-test on the first fix)** closed the two remaining formation failures. Convergence speed: a self-dial rejection now retries the seed on a short fixed interval (500ms) instead of walking the exponential backoff — the backoff treated an expected ~1/N outcome of a load-balanced seed as seed failure, stretching one pod's convergence past 60s. Lost `DIRECTOR-ADD` under concurrent formation: every membership-changing path now broadcasts over the *pre-reconcile* connection set before recomputing its right neighbor (`reconcile()` could tear down a live connection the announcement still needed, permanently stranding a directly-connected member at a stale view). And a periodic anti-entropy snapshot (`components.director.anti_entropy_interval`, default 3s) re-broadcasts the member+tombstone `DIRECTOR-LIST` over every live ring connection as a bounded safety net — any split with at least one crossing connection heals within one interval.

**#759 second follow-up (live re-test showed formation still partitioning)** closed the last structural gap: fully disjoint subrings with zero crossing connections, which every connection-bound mechanism above is architecturally blind to. Under concurrent formation each node's single right-neighbor dial is computed from its own divergent view, so the dial graph isn't guaranteed connected — and the one guaranteed crossing point (the shared ClusterIP seed) used to be contacted exactly once. The seed poll is now periodic (`components.director.seed_poll_interval`, default 2s; negative restores one-shot): every member keeps re-fetching the seed's member+tombstone snapshot through the same idempotent union merge, bounding any formation partition's lifetime by the poll interval regardless of dial topology. Re-joins from known members are served as read-only snapshot requests (no `DIRECTOR-ADD` storm). Gated in-process by a formation test that joins N members simultaneously through a mock load-balanced seed with random routing (including self-dials).

**#759 third follow-up (live re-test: healing worked but took ~2 minutes, failing the converge-in-seconds gate)** removed the two latency sources. Poll pacing now gates on the configured cluster target size instead of own-view stability: full cadence while the view holds fewer than `min_members`, easing to `seed_poll_idle_interval` once the expected size is reached (and snapping back on any loss) — the previous stable-view backoff slowed exactly the node that needed healing, because a partitioned node's own view looks perfectly stable. And a hostname seed is now resolved explicitly with the poll fanned out to every resolved address except self each cycle: with the headless `-director-ring` Service as the seed this is a deterministic sweep of all peers (convergence in about one poll interval, zero self-dials) and sidesteps Go's RFC 6724 own-IP-first address ordering that a naive dial of a headless DNS answer hits. A literal-IP or load-balanced seed keeps the previous behavior (server-side self-dial rejection + 500ms fast retry) as the fallback path.

**#764 (live breakthrough after the DNS fan-out landed)** made the chart default the ring seed to the headless Service so the fix can't be defeated by configuration. With the fan-out in place, formation converged with the headless `-director-ring` seed (DNS → all pod IPs) but still partitioned with the ClusterIP `-director` seed (DNS → one virtual IP → load-balanced random routing → self-dials). The seed is now auto-derived to `<release>-director-ring:<directorPort>` whenever `components.director.peers` is empty and `replicas > 1`, so a fresh `replicas=3` install converges with no manual seed and no way to accidentally point the ring at the ClusterIP. `-director` remains the login-pod LOOKUP endpoint only.

**#768 (supersedes the brief all-probes-all liveness)** realigned death detection to the reference's both-neighbors model: a death is detected by the dead node's immediate neighbors — O(1) probes per node regardless of ring size — never by an O(N²) everyone-probes-everyone sweep. The right side was already covered by the dial path; the **left side** now treats losing the accepted connection from the current left neighbor as a death signal too (verified with a few short-deadline probes before declaring, so a benign dial re-target isn't mistaken for a death) — previously the N=2 higher-sorted member, who never dials, was permanently blind to its only peer's death. Both ends of every ring connection also exchange `PING`/`PONG` keepalives on `ping_interval` with a `ping_interval + ping_timeout` read deadline, so a silently-hung peer surfaces as a read error feeding the same death paths; and every deliberate teardown announces itself with `QUIT\t<reason>` (reference parity), so a benign dial re-target is classified instantly instead of via probes. Phantom members from rolling-restart churn converge via anti-entropy spreading the phantom until exactly one node computes it as its right neighbor, dials it, fails, and evicts it for everyone. Tombstones carry a TTL (`components.director.tombstone_ttl`, default 600s) so churn across many rollouts can't grow the set unboundedly — safe because a resurrected-but-unreachable member is re-evicted by its neighbors within seconds regardless.

**#772 (userDir state exchange — snapshot base)** starts closing the routing-STATE half of the ring (the membership half being done). On every ring (re)connect, right after the member `DIRECTOR-LIST`, a director now streams its **userDir snapshot** — `USER\t<hash>\t<backend>\t<seq>\t<by>\t<weak>` per sticky assignment — so a fresh or restarted director inherits current sticky routing state immediately instead of starting empty and re-deriving only from the hash. Assignments carry a **Lamport-clock** stamp `(assign_seq, assign_by)`, not wall-clock (pod clocks are unsynchronized; unix-nano would let the fastest-clock replica win nondeterministically) — a strictly higher seq wins, a tie breaks to the lower director id, deterministic and test-reproducible. A fresh sticky assignment made by a normal `LOOKUP` also propagates live around the ring as a `USER-ASSIGN` event (only the new pin — not the sticky TTL-refresh, which would make every repeat login a broadcast), director↔director by hash, applied under the same Lamport order. When a merge moves a user to a different backend — a same-`assign_seq` conflict where the lower id won and this director lost, or a newer reassignment — the loser kicks its own now-stale sessions for that user off the wrong backend (`USER-KICKED` to the owning login proxy), so a mailbox is never split across two backends; other users on that backend keep running. This completes the #772 userDir state exchange: sticky routing is now consistent across replicas even where it diverges from the deterministic hash, closing the replication half of #708 and the divergence part of #706.

**#770 (graceful leave on SIGTERM)** removes the death-detection latency from planned exits. Before shutting down, a director announces `DIRECTOR-REMOVE` for itself around the ring (peers evict it instantly via the existing tombstone path — zero probe window), sends `QUIT` on every ring connection, and rejects further JOINs (`JOIN-FAIL\tshutting down`) so no fresh joiner can learn the dying member. A hard kill still converges via the #768 neighbor-monitoring path — graceful leave only optimizes the expected k8s rolling-restart / scale-down case.

**#765 (residual after the seed fix: a respawned pod held a recently-dead member)** made the settled-cadence backoff safe by default. Killing a pod on a converged ring respawns a fresh pod that could learn the dying member as *live* during the death-detection window, reaching `min_members` with a stale entry — at which point the previous lazy 30s idle cadence left the dead member in place for up to 30s (live-measured 40s+). The idle cadence is now a separate knob `components.director.seed_poll_idle_interval`, **defaulting to the same 2s as the active cadence** (no effective backoff), because a node cannot locally distinguish "converged" from "stable but holding a dead member". Operators can raise it to trade steady-state polling for slower dead-member eviction; the tombstone-wins merge and per-`DIRECTOR-LIST` tombstone exchange (#754) already guaranteed correctness — this only bounds the latency.
