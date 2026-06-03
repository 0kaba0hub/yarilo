# OAuth 2.0 / OAUTHBEARER + XOAUTH2 Integration

`yarilo-auth` validates OAuth 2.0 bearer tokens for the SASL
**OAUTHBEARER** (RFC 7628) and **XOAUTH2** (Google/Microsoft legacy
extension) mechanisms and exposes them as a passdb so OAuth logins
flow through the same chain machinery as SQL logins (cache, penalty,
policy, audit log — all work unchanged).

Both mechanisms share the same token validator. The only difference
is the wire format of the client's initial response.

## When to enable

A token-based login is useful when:

- the deployment integrates with Google Workspace, Microsoft 365,
  Keycloak, Authelia or any OIDC-compliant IdP
- mailbox passwords should live entirely outside the mail server
  (no SQL passdb at all, or SQL-only as fallback)
- per-mailbox app passwords are replaced by IdP-issued tokens

Skip when the threat model is satisfied by a simple SQL passdb and
the operator has no OIDC infrastructure.

## Configuration

`auth.oauth2` in `yarilo.yaml` (or `components.auth.oauth2` in Helm
`values.yaml`) is a list. Each entry becomes one passdb in the
auth chain, placed **ahead of SQL** so an OAUTHBEARER login
resolves through the validator before SQL ever sees the bearer
token as a plaintext password.

Four validation modes:

| Mode | Required fields | Notes |
|:---|:---|:---|
| `local` | `jwks_url` | JWT signature verify against a cached JWKS. No HTTP call per login. RECOMMENDED when the IdP issues signed JWTs (Google, Microsoft, most OIDC providers). |
| `introspection` | `introspection_url`, usually `client_id` + `client_secret` | RFC 7662 introspection. Supports opaque (non-JWT) tokens. Slower (one HTTP call per login). |
| `tokeninfo` | `tokeninfo_url` | Google-style endpoint (`?access_token=…`). Pre-OIDC providers. |
| `discovery` | `issuer_url` | Auto-resolves `jwks_uri` + `introspection_endpoint` from `<issuer>/.well-known/openid-configuration`. PREFERRED for new deployments — least operator config. |

### Common fields (apply across modes)

| Field | Default | Notes |
|:---|:---|:---|
| `issuers` | `[]` | Allow-list of `iss` claim values. Empty disables the check. In `discovery` mode the document's `issuer` is auto-added for `local` mode. |
| `audience` | `""` | Required `aud` claim value. Empty disables. JWT spec allows `aud` to be a string array — match against any entry passes. |
| `scopes` | `[]` | Scopes every token MUST carry (intersection check). |
| `username_attribute` | `email` | Claim name resolving to the mail user. |
| `username_validation_format` | `%{user}` | Template applied to the SASL authzid before comparison. Supports `%u`, `%{user}`, `%Lu`, `%n`, `%Ln`, `%d`, `%Ld`. |
| `active_attribute` | `""` | Optional claim name that must be present. Disables check when empty. |
| `active_value` | `""` | When non-empty, the claim's value must equal this string. |
| `extra_fields` | `[]` | Claim names whose values are projected onto the auth response as `userdb_<claim>` fields. |
| `token_expire_grace_seconds` | `60` | Clock-skew tolerance after the token's `exp`. |
| `http_timeout_ms` | `5000` | Round-trip cap for introspection / tokeninfo / discovery / JWKS refresh. |

### Introspection sub-modes

When `mode: introspection` (or `discovery` with introspection
selected), `introspection_mode` picks the transport:

| Value | Transport |
|:---|:---|
| `post` (default) | RFC 7662: POST `application/x-www-form-urlencoded` with `token=<token>` in the body |
| `auth` | POST with `Authorization: Bearer <token>` header, empty body |
| `get` | GET `<url>?token=<token>` |

## Wire shapes

### OAUTHBEARER (RFC 7628, recommended)

Client sends a GS2-wrapped bearer token in the initial response:

```
n,a=alice@example.com,\x01host=mail.example.com\x01port=993\x01auth=Bearer <token>\x01\x01
```

Fields are separated by `\x01` (ASCII SOH). The `host=` and
`port=` fields are optional; field order is flexible.

### XOAUTH2 (Google/Microsoft legacy)

Client sends a simpler format — no GS2 header, no host/port:

```
user=alice@example.com\x01auth=Bearer <token>\x01\x01
```

Fields are `\x01`-separated. The `Bearer ` prefix on `auth=` is
case-insensitive. XOAUTH2 does not support channel binding.

**When to use XOAUTH2:** Legacy Outlook configurations, older
Android mail apps, and some older Thunderbird OAuth setups that
advertise XOAUTH2 instead of OAUTHBEARER. New clients should
prefer OAUTHBEARER. Both mechanisms hit the same token validator
— there is no difference in security posture once a TLS session
is established.

Both mechanisms validate the token per the configured provider and
either succeed or return a JSON failure descriptor
(`{"status":"invalid_token","schemes":"bearer"}`).

### Mechanism advertisement

Both `OAUTHBEARER` and `XOAUTH2` are added to the SASL capability
list on every protocol that supports them:

- **IMAP** — `AUTH=OAUTHBEARER` and `AUTH=XOAUTH2` in `CAPABILITY`
- **POP3** — `CAPA` lists `SASL OAUTHBEARER XOAUTH2`
- **Submission (SMTP)** — EHLO `AUTH` extension lists both

Advertisement is gated by `auth.oauth2` being non-empty: a
deployment that configures no OAuth providers does NOT advertise
either mechanism, so a client never picks a mech the server
cannot validate.

### Fast-fail on rejection

RFC 7628 §3.2.3 mandates a two-round failure handshake — the
server returns the JSON error blob with `done=false`, the client
acknowledges with a 0x01 dummy byte, and the server then closes
the SASL exchange. Real-world Go clients (`go-imap`,
`go-smtp` `imapclient`) skip the dummy step: their `saslClient.Next`
returns the error immediately and the protocol-layer loop
unwinds, leaving the server blocked on a read that never
arrives until the IMAP idle timeout (5+ minutes).

Yarilo's OAUTHBEARER server returns `done=true` on the first
rejection. The JSON error blob is still surfaced as the final
challenge so the client sees the proper RFC 7628 failure
descriptor; the protocol read on the server side completes
immediately rather than hanging.

This is a deliberate deviation from the spec's failure
choreography. Compliant clients that DO send the 0x01 dummy will
see the SASL exchange close one round-trip earlier than they
expect, but no client we've tested misbehaves on this.

## Worked examples

### Google Workspace (discovery mode)

```yaml
auth:
  oauth2:
    - mode: discovery
      issuer_url: https://accounts.google.com
      audience: "1234567890-abcdef.apps.googleusercontent.com"
      scopes: [openid, email]
      username_attribute: email
      username_validation_format: "%Lu"
      extra_fields: [sub, hd]
```

- `issuers` auto-resolved from the discovery document.
- `audience` is the OAuth client ID from Google Cloud Console.
- `username_validation_format: %Lu` accepts a mixed-case SASL
  authzid against a lowercased `email` claim.
- `extra_fields: [hd]` projects the Google "hosted domain" claim
  as `userdb_hd` so downstream rules (ACL, quota presets) can
  branch on it.

### Microsoft 365 (local JWT mode)

```yaml
auth:
  oauth2:
    - mode: local
      jwks_url: https://login.microsoftonline.com/<tenant>/discovery/v2.0/keys
      issuers:
        - https://login.microsoftonline.com/<tenant>/v2.0
      audience: "<azure-app-id>"
      scopes: [openid, email, profile]
      username_attribute: preferred_username
      extra_fields: [oid, tid, roles]
```

- `username_attribute: preferred_username` — Microsoft's email is
  in this claim, not `email`.
- `extra_fields: [roles]` projects Azure AD app roles for
  downstream policy.

### Keycloak / Authelia (introspection mode)

```yaml
auth:
  oauth2:
    - mode: introspection
      introspection_url: https://keycloak.example/realms/yarilo/protocol/openid-connect/token/introspect
      introspection_mode: post
      client_id: yarilo-auth
      client_secret: "{{ .Values.secrets.keycloakClient }}"
      issuers: [https://keycloak.example/realms/yarilo]
      audience: yarilo-auth
      username_attribute: email
      active_attribute: active
      active_value: "true"
      extra_fields: [sub, realm_access]
```

- Keycloak's introspection endpoint requires client credentials
  HTTP Basic auth.
- `active_attribute: active` + `active_value: "true"` enforces the
  RFC 7662 `active` field explicitly even when client + server
  versions disagree on the default.

### Chained provider order

The OAuth chain runs **before** SQL. The order within
`auth.oauth2` is honoured — list the strictest / cheapest provider
first:

```yaml
auth:
  oauth2:
    - mode: local        # try cached JWKS first
      jwks_url: https://idp.example/jwks.json
    - mode: introspection  # fall through to introspection for opaque tokens
      introspection_url: https://idp.example/introspect
      client_id: yarilo
      client_secret: …
  passdb:
    - driver: sqlite     # SQL fallback for app passwords
      dsn: /var/lib/yarilo/users.db
```

A login that supplies an opaque token falls through `local` (no
matching signature → `ResultNext`) and lands in `introspection`.
A login with a username + plaintext password falls through both
OAuth entries (no bearer token → `ResultNext`) and reaches SQL.

## Error mapping

Validator-level errors translate to chain results:

| Validator error | Chain result | Effect |
|:---|:---|:---|
| `ErrUpstream` (network / 5xx) | `ResultTempFail` | Caller maps to `temp_fail`; failure-delay / penalty apply |
| `ErrTokenExpired` / `ErrTokenInvalid` / `ErrTokenInactive` | `ResultNext` | Chain continues to the next passdb |
| `ErrUsernameMismatch` / `ErrIssuerMismatch` / `ErrAudienceMismatch` / `ErrScopeMissing` / `ErrInactiveAccount` | `ResultNext` | Same — let SQL try the credential as a password |

This means an OAUTHBEARER login that fails validation
**transparently falls through to SQL** — a deployment that
mixes OAuth and legacy SQL accounts handles both with one chain.

## Privacy notes

- `local` mode never sends the bearer token to a third party.
  Only the IdP's public JWKS is fetched (no token leaves the auth
  pod).
- `introspection` / `tokeninfo` / `discovery` send the token to
  the configured endpoint. Use HTTPS endpoints exclusively.
- The auth-cache (`auth.cache.size_bytes > 0`) caches validated
  tokens as HMAC, not plaintext. Set
  `auth.cache.ttl_seconds` shorter than the token's typical `exp`
  so revocation propagates within one cache window.

## Tuning notes

- For `local` mode, the JWKS auto-refreshes every hour and on
  signature-verify miss (unknown `kid`). Key rotation is handled
  transparently.
- For `introspection` / `tokeninfo` / `discovery`, pair with
  `auth.cache` to avoid one HTTP call per IMAP IDLE reconnect.
- `token_expire_grace_seconds: 60` covers normal clock skew. Set
  higher (300+) when the IdP and the auth pod are in different
  data centres with poor NTP discipline.

## Client smoke-test recipes

Acquire a bearer token from the IdP (Google: `oauth2l fetch`;
Azure: `az account get-access-token`; Keycloak: `kcadm.sh` or
direct token endpoint POST). Then:

### `swaks` (Submission)

```sh
TOKEN="$(oauth2l fetch --scope='https://mail.google.com/' …)"
swaks --server mail.yarilo.example:587 \
      --tls --tls-verify \
      --auth OAUTHBEARER --auth-user alice@example.com \
      --auth-password "$TOKEN" \
      --from alice@example.com --to bob@example.com
```

### `openssl s_client` + raw SASL (IMAP / POP3)

```sh
TOKEN="…"
PAYLOAD=$(printf 'n,a=alice@example.com,\1auth=Bearer %s\1\1' "$TOKEN" | base64 -w0)

# IMAP
openssl s_client -quiet -connect mail.yarilo.example:993 <<EOF
a1 AUTHENTICATE OAUTHBEARER $PAYLOAD
a2 SELECT INBOX
a3 LOGOUT
EOF

# POP3
openssl s_client -quiet -connect mail.yarilo.example:995 <<EOF
AUTH OAUTHBEARER $PAYLOAD
STAT
QUIT
EOF
```

### Thunderbird

Settings → Account → Authentication method → **OAuth2**. On the
first connection Thunderbird opens the IdP's consent page;
subsequent reconnects refresh the access token automatically.
