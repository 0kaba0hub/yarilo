# Auth Penalty (cross-pod IP backoff)

`yarilo-auth` can enforce a per-client-IP exponential backoff on
failed auth attempts. The counter lives in the `yarilo-warden`
service so a single attacker IP pays the cost across every auth pod
they land on — pod load-balancing does not let them reset by
landing on a fresh pod.

## When to enable

A penalty store is useful when:

- the deployment runs more than one `yarilo-auth` pod and the
  process-local `auth_failure_delay` no longer covers all paths
- the threat model includes credential stuffing from a single IP
  (or a small IP set) — exponential backoff makes 100 attempts/sec
  unprofitable
- `auth_failure_delay` alone amplifies legitimate retries too
  aggressively (delay applies to every attempt; penalty escalates
  only after sustained failure)

When the deployment is one pod and `auth_failure_delay: 2` already
covers the threat surface, the penalty store is overkill.

## Configuration

`auth.penalty` in `yarilo.yaml` (or `components.auth.penalty` in
Helm `values.yaml`):

```yaml
auth:
  penalty:
    enabled: true
```

The penalty store reuses the configured `warden_service.listen`
address; no separate connection knob.

| Setting | Default | Notes |
|---|---|---|
| `enabled` | `false` | Opt-in. While `false`, every login skips both Lookup and Update (zero overhead). Flip to `true` once `yarilo-warden` is provisioned |

## Behaviour

- **Pre-passdb**: Lookup the counter for the client IP. Sleep
  `PenaltyToSecs(count)` seconds before running the chain.
- **Post-passdb**:
  - `ResultOK` → Update(ip, 0) — reset to zero on success
  - `ResultFail` → Update(ip, count+1) — capped server-side at
    `MaxPenalty` (4)
  - `ResultTempFail` (passdb backend down) → **no update** —
    a backend outage is not the client's fault, and counting
    those would lock every client out for the decay window
- **Master-user flows exempt** — admin sessions are never
  tarpitted regardless of unrelated IP noise

## Backoff curve

| Counter | Sleep before next attempt |
|---:|---:|
| 0 | 0s |
| 1 | 2s |
| 2 | 4s |
| 3 | 8s |
| 4+ | 15s (cap) |

Cumulative budget for the first four failures from a clean IP:
`0 + 2 + 4 + 8 = 14s`. The fifth failure waits the cap (15s); the
sixth, seventh, … each also wait the cap.

## Decay

After 29 seconds with no Update, the entry is swept (matches the
cumulative budget plus one cap window — once the worst-case backoff
chain has fully played out, the entry no longer carries useful
signal). Configurable via `warden.WithPenaltyDecay` for testing,
not exposed in the YAML — the default is well-calibrated.

A decayed entry returns 0 on the next Lookup, so a long-quiet IP
starts fresh on its next attempt.

## Wire protocol

`yarilo-warden` exposes two new verbs in protocol version 1.6:

```
PENALTY-LOOKUP <ip>            → PENALTY <count>
PENALTY-UPDATE <ip> <count>    → OK
```

- `count` is clamped server-side to `[0, MaxPenalty]`
- `count=0` deletes the entry (auth-success reset)
- `count<0` clamps to 0; `count>MaxPenalty` clamps to MaxPenalty

The full warden protocol is documented in
`internal/warden/server.go`.

## Interaction with other anti-abuse layers

The defence-in-depth stack is layered:

1. **TCP / TLS layer**: HAProxy rate limits, fail2ban — pre-Yarilo
2. **Connection limits**: `mail_max_userip_connections` enforced
   by `yarilo-warden` (`CONNECT` / `DISCONNECT` verbs)
3. **Auth failure delay**: `auth.failure_delay` (2s by default)
   equalises timing between unknown-user and wrong-password
4. **Auth penalty** (this feature): exponential backoff per IP,
   shared cross-pod via `yarilo-warden`
5. **Policy server** (`auth.policy.url`): external HTTP hook with
   pattern-detection logic — see [AUTH_POLICY.md](AUTH_POLICY.md)

Penalty + failure-delay compose additively: an attacker on
counter=2 facing `failure_delay=2s` waits `4s (penalty) + 2s
(delay) = 6s` per attempt. Tune the two together.

Penalty + policy compose orthogonally: penalty is built-in and
cheap; policy is external and rich. Operators typically enable
penalty first (zero infrastructure) and add policy only when they
need pattern-detection beyond per-IP counts.
