# SCRAM family (SHA-256 and SHA-1, each with optional PLUS)

`yarilo-auth` supports the Salted Challenge Response Authentication
Mechanism family (RFC 5802 / RFC 7677) as a SASL mechanism in IMAP,
POP3 and Submission. SCRAM is the recommended replacement for
plain-text + TLS auth: the server never sees the user's password,
and the client proves possession through a salted HMAC ladder
that resists offline brute-force on the wire.

The PLUS variant adds **channel binding** (RFC 9266 `tls-exporter`)
so a successful auth additionally proves the client is on the
same TLS session — defeating MITM proxies that terminate TLS at
the attacker.

## Supported mechanisms

| Mechanism | RFC | Use |
|:---|:---|:---|
| `SCRAM-SHA-256` | RFC 7677 | **Default for new deployments.** PBKDF2-HMAC-SHA-256 ladder. |
| `SCRAM-SHA-256-PLUS` | RFC 7677 + RFC 9266 | SHA-256 + channel binding via TLS exporter. |
| `SCRAM-SHA-1` | RFC 5802 | Compatibility with legacy clients (older Thunderbird builds, Apple Mail fallback). PBKDF2-HMAC-SHA-1 ladder. |
| `SCRAM-SHA-1-PLUS` | RFC 5802 + RFC 9266 | SHA-1 + channel binding. |

SHA-1 is **not deprecated for SCRAM** — the HMAC construction is
not broken by SHA-1 collision attacks the way digital signatures
are. It is provided strictly for client compatibility; new
deployments should provision SHA-256 verifiers. The two digest
families are independent — a single user may carry one or the
other (but only one verifier blob per user; clients negotiate
the strongest mech the server advertises).

## When to enable

Enable SCRAM when:

- Modern MUAs are on the deployment path (Thunderbird, K-9 Mail,
  any client that supports SCRAM out of the box).
- The threat model includes attackers with intermittent network
  access (SCRAM denies them the password; plain-text-over-TLS
  hands them the password the moment they get a single MITM
  window).
- Compliance pressure to never store plain passwords on the
  server (SCRAM verifiers are derived material — operators
  cannot recover the original password).

Skip when every client is restricted to PLAIN/LOGIN, or when the
operator cannot run the one-time verifier-generation step per
user.

## Storage format

Each SCRAM-enabled user's `password` column carries a Dovecot-
compatible blob — the inner shape is identical across the
SHA-256 and SHA-1 families; only the scheme prefix differs:

```
{SCRAM-SHA-256}<iterations>,<base64-salt>,<base64-stored-key>,<base64-server-key>
{SCRAM-SHA-1}<iterations>,<base64-salt>,<base64-stored-key>,<base64-server-key>
```

Example:

```
{SCRAM-SHA-256}600000,gJ4oG0RpUmMrzFQ9PAGUng==,...,...
{SCRAM-SHA-1}600000,gJ4oG0RpUmMrzFQ9PAGUng==,...,...
```

The fields:

- `iterations` — PBKDF2-HMAC-SHA-256 iteration count.
- `salt` — per-user random salt (16 bytes recommended).
- `stored_key` = `SHA-256(HMAC(salted_password, "Client Key"))`.
- `server_key` = `HMAC(salted_password, "Server Key")`.

`salted_password = PBKDF2(password, salt, iterations)` — that
intermediate is NEVER stored. The plain password is never stored
in any form.

### Same blob serves PLAIN and SCRAM

Both auth flows interpret the same column:

- **PLAIN/LOGIN**: server re-derives `stored_key` from the
  incoming plain password using the column's iter+salt, then
  constant-time compares against the stored `stored_key`. The
  plain password never leaves the connection's goroutine.
- **SCRAM-SHA-256 / -PLUS**: server uses `stored_key` +
  `server_key` directly to drive challenge-response. Plain
  password is never involved.

Operators register a user once and the same row is consumable by
both flows. Mixed-MUA deployments do not need separate password
columns per protocol.

## Generating a verifier

```sh
yarilo-admin auth scram-verifier --password 'hunter2'
# → {SCRAM-SHA-256}600000,…

yarilo-admin auth scram-verifier --mech sha1 --password 'hunter2'
# → {SCRAM-SHA-1}600000,…
```

`--mech` defaults to `sha256`. Use `sha1` only when a target
client cannot speak SHA-256.

`--iterations N` overrides the default 600 000 (2023 OWASP
recommendation for SHA-256-based PBKDF2; the same default applies
to SHA-1 because the iteration count is what carries the
compute-cost — the digest choice is fixed by the mech). Anything
below 4096 is clamped up so a typo cannot land a weak verifier in
the database.

Omit `--password` to read it from stdin (one line).

The output is the literal value to drop into the SQL `password`
column. Existing PLAIN-or-BCRYPT users keep working unchanged —
they just don't get SCRAM advertisement.

## Mechanism advertisement

`yarilo` advertises each SCRAM mech only when at least one
configured passdb exposes verifiers in that digest family. The
type assertion is per-family:

- `SCRAM-SHA-256` / `SCRAM-SHA-256-PLUS` light up when the
  Authenticator implements `SCRAMSha256Lookup`.
- `SCRAM-SHA-1` / `SCRAM-SHA-1-PLUS` light up when the
  Authenticator implements `SCRAMSha1Lookup`.

A deployment that provisions only SHA-256 verifiers never
advertises the SHA-1 mechs, and vice versa. Discovery happens
at session-setup time per connection:

- **IMAP** — matching mechs appear in the `CAPABILITY` reply.
- **POP3** — `CAPA` lists them after `SASL`.
- **Submission (SMTP)** — EHLO's `AUTH` extension lists them
  alongside `PLAIN LOGIN`.

The `-PLUS` variants are additionally gated on the TLS state
having the RFC 9266 exporter available (TLS 1.3+ over a real
`*tls.Conn`). Plain TCP or TLS 1.2 sessions advertise only the
non-PLUS mechs.

## Wire shape (RFC 5802 §5)

Three-round exchange:

```
client-first  → "n,,n=alice,r=<client-nonce>"
                (or "p=tls-exporter,," for the PLUS variant)
server-first  ← "r=<combined-nonce>,s=<base64-salt>,i=<iter>"
client-final  → "c=<base64-cb>,r=<combined-nonce>,p=<base64-proof>"
server-final  ← "v=<base64-server-signature>"
```

The server-final `v=` carries an HMAC that the client verifies
to confirm the server itself knew the verifier — mutual
authentication without a second round-trip.

## Channel binding policy

Identical for both digest families:

- **Non-PLUS** (`SCRAM-SHA-256`, `SCRAM-SHA-1`): client MUST send
  `n,,` or `y,,`. `p=…` is rejected because the client should
  have picked the PLUS mechanism if it has channel binding.
- **PLUS** (`SCRAM-SHA-256-PLUS`, `SCRAM-SHA-1-PLUS`): client
  MUST send `p=tls-exporter,,`. `n,,` and `y,,` are downgrade
  attempts and rejected.

The `y,,` value lets a TLS client SIGNAL that it knows about
channel binding (so a wiretap attacker can be detected by a
later trace) without actually using it. We accept `y,,` —
operators who require strict channel binding configure clients
to use the PLUS mechanism.

## Defence against user enumeration

When `LookupSCRAMSha256` returns `(nil, nil)` (unknown user),
the SCRAM server **does not short-circuit**. It generates fake
iter+salt+zero StoredKey and drives the full exchange through
`client-final`; the proof check then fails uniformly because no
client can derive a proof against a zero StoredKey.

The result: an attacker cannot tell apart "user does not exist"
from "user exists, wrong password" by timing alone.

## Iteration count guidance

| Year | Recommended minimum (SHA-256) |
|:---|:---|
| 2017 (RFC 7677) | 4 096 |
| 2023 (OWASP) | **600 000** ← Yarilo default |

Tooling clamps anything below 4 096 up to 4 096 so a misconfig
never lands trivially-cracked verifiers in the database.
Operators on hardware that struggles with 600 000 can run
`yarilo-admin auth scram-verifier --iterations N` to pick a
lower value (down to the 4 096 floor) — but this is a
deliberate security trade-off and should be documented in the
deployment's runbook.

## Smoke-test recipes

### `openssl s_client` + raw SASL (IMAP)

```sh
openssl s_client -quiet -connect mail.yarilo.example:993 -tls1_3 <<'EOF'
a1 CAPABILITY
EOF
# Confirm CAPABILITY lists AUTH=SCRAM-SHA-256 and (over TLS 1.3) AUTH=SCRAM-SHA-256-PLUS
```

For the full multi-round exchange use a SCRAM-capable client
(e.g. Thunderbird, or the Go `pkg.go.dev/github.com/xdg-go/scram`
library) — driving SCRAM by hand against openssl is tedious
because every round needs separate base64 work.

### Thunderbird

Settings → Account → Authentication method → **Encrypted
password (Normal password)**. Thunderbird auto-selects SCRAM-SHA-256
when both the server and the configured profile advertise it.
For SCRAM-SHA-256-PLUS, ensure the connection is TLS 1.3 (Tools →
Options → Privacy & Security → Certificates) and the server
exposes the `+PLUS` capability.

## Tuning notes

- The verifier derivation runs once per user-registration; the
  per-login cost is only HMAC + SHA-256 + memcmp — negligible
  relative to PBKDF2 of 600 000 rounds. Raise iterations as
  hardware budgets grow without runtime penalty.
- SCRAM does not benefit from the `auth.cache` (the SCRAM
  exchange itself replaces what the cache would shortcut). Leave
  the cache configured for PLAIN/LOGIN/OAUTHBEARER traffic.
- For multi-tenant deployments, rotate verifiers per-tenant by
  re-running `scram-verifier` and pushing fresh blobs through
  the SQL passdb's update path. There is no cluster-wide
  verifier rotation primitive — verifiers are per-user.
