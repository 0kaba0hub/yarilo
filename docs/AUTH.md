# Authentication configuration

Yarilo authenticates IMAP/POP3/Submission credentials against an ordered chain of `passdb` entries. The first passdb that returns a definitive result (success or failure for a known user) wins. Unknown users fall through to the next entry.

---

## `auth.passdb`

A list of passdb entries. Each entry has a `driver` and a `dsn`. Order matters — entries are tried left-to-right.

| Key | Description |
|:---|:---|
| `driver` | Backend type: `sqlite` \| `mysql` \| `postgres` \| `passwd-file`. |
| `dsn` | SQL drivers: driver-specific connection string. `${ENV_VAR}` is expanded at startup. |
| `passwd_file` | `passwd-file` driver: path to the user file. `${ENV_VAR}` is expanded at startup. |
| `password_query` | SQL: optional custom SELECT for authentication. Defaults to the built-in `yarilo_users` schema. See [Custom queries](#custom-queries). |
| `user_query` | SQL: optional separate userdb lookup (`home`, `mail`). When unset, userdb fields come from `password_query`. |
| `iterate_query` | SQL: optional list-users query for admin tooling. |
| `default_pass_scheme` | Assumed scheme when stored password has no `{SCHEME}` prefix and no crypt(3) marker. Default: `PLAIN` (SQL), `CRYPT` (passwd-file). |
| `skip_schema` | SQL: `true` to skip `CREATE TABLE IF NOT EXISTS yarilo_users` on startup — use when connecting to an existing schema. |

```yaml
auth:
  passdb:
    - driver: sqlite
      dsn: /var/lib/yarilo/users.db
```

---

## SQL passdb

All three SQL backends share one schema:

```sql
CREATE TABLE yarilo_users (
    username  TEXT/VARCHAR(255) PRIMARY KEY,
    password  TEXT/VARCHAR(255) NOT NULL,
    home      TEXT/VARCHAR(255) NOT NULL DEFAULT '',
    mail      TEXT/VARCHAR(255) NOT NULL DEFAULT '',
    enabled   INTEGER/TINYINT(1) NOT NULL DEFAULT 1
);
```

Yarilo runs `CREATE TABLE IF NOT EXISTS` on startup — fresh installs work without manual migration. Existing tables are left untouched.

### SQLite

Pure-Go driver (`modernc.org/sqlite`), no cgo. Best for single-node deployments and dev environments.

```yaml
auth:
  passdb:
    - driver: sqlite
      dsn: /var/lib/yarilo/users.db
```

### MySQL / MariaDB

DSN format (`go-sql-driver/mysql`): `user:password@tcp(host:3306)/dbname?charset=utf8mb4&parseTime=true`.

```yaml
auth:
  passdb:
    - driver: mysql
      dsn: "yarilo:${DB_PASSWORD}@tcp(db.internal:3306)/yarilo?charset=utf8mb4"
```

### PostgreSQL

DSN format (`pgx`): standard `postgres://user:password@host:5432/dbname?sslmode=require` URL.

```yaml
auth:
  passdb:
    - driver: postgres
      dsn: "postgres://yarilo:${DB_PASSWORD}@db.internal:5432/yarilo?sslmode=require"
```

---

## passwd-file passdb

A flat, colon-separated user file (classic `/etc/passwd` layout) that serves
**both** passdb and userdb roles — a self-hosted deployment can authenticate
and resolve mail storage without any database.

```yaml
auth:
  passdb:
    - driver: passwd-file
      passwd_file: /etc/yarilo-passwd/passwd
      default_pass_scheme: CRYPT
```

> **Kubernetes note.** Mount the file from a Secret/ConfigMap into its **own**
> directory — do **not** place it under `/etc/yarilo`, which is already the
> rendered-config mount. A `subPath` file mounted inside that directory fails
> with `not a directory` and crash-loops every pod. With the Helm chart, supply
> the content via `extraVolumes` / `extraVolumeMounts` at a distinct path
> (e.g. `/etc/yarilo-passwd`) and point `passwd_file` at it:
>
> ```yaml
> extraVolumeMounts:
>   - name: passwd-file
>     mountPath: /etc/yarilo-passwd
>     readOnly: true
> extraVolumes:
>   - name: passwd-file
>     secret:
>       secretName: yarilo-passwd-file
> ```

### File format

One user per line:

```
user:password:uid:gid:gecos:home:shell:extra_fields
```

- Only `user` and `password` are required.
- `uid` / `gid` / `gecos` / `shell` are parsed for layout compatibility but
  **ignored** — yarilo derives privilege drop from config and storage paths from
  the home template, not per-user uid/gid.
- `home` (column 6) populates the userdb `home` field.
- `extra_fields` (column 8 onward) are space-separated `key=value` pairs.
  Keys with a `userdb_` prefix populate the userdb (`userdb_mail`,
  `userdb_quota_rule`, …); bare keys are passdb-side (`allow_nets`, `nologin`, …).
- Lines that are empty or begin with `#` or `:` are skipped.
- The file is reloaded automatically when its mtime or size changes.

Passwords carry a `{SCHEME}` prefix or a crypt(3) marker; unmarked values assume
`default_pass_scheme` (default `CRYPT` = crypt(3) autodetection). The same
scheme set as the SQL passdb applies, including `{SCRAM-SHA-256}` verifiers.

```
# comment
alice@example.com:{BCRYPT}$2b$12$...:1000:1000::/mail/alice::userdb_mail=maildir:~/Maildir userdb_quota_rule=*:storage=2G
bob@example.com:{SHA512-CRYPT}$6$salt$hash
```

---

## static passdb

One shared credential and a set of templated fields applied to **every** user.
Serves both passdb and userdb roles. For tests, single-mailbox installs, and
proxy front-ends. Because it matches every username it must be placed **last**
in the chain.

```yaml
auth:
  passdb:
    - driver: sqlite
      dsn: /var/lib/yarilo/users.db
    - driver: static            # catch-all — last in the chain
      static_password: "${YARILO_STATIC_PASSWORD}"
      default_pass_scheme: BCRYPT
      fields:
        userdb_home: "/var/vmail/%d/%n"
        userdb_mail: "maildir:/var/vmail/%d/%n/Maildir"
```

| Key | Description |
|:---|:---|
| `static_password` | Shared password (`{SCHEME}` prefix or `default_pass_scheme`). `${ENV_VAR}` expanded at startup. |
| `nopassword` | `true` accepts **any** password — for proxy front-ends where the upstream authenticates. Mutually exclusive with `static_password`. |
| `fields` | Templated user fields. Values expand `%u` / `%n` / `%d`. `userdb_`-prefixed keys populate the userdb; bare keys are forwarded on the passdb path (`allow_nets`, `proxy`, …). |

Proxy front-end (accept any credential, let the backend verify):

```yaml
auth:
  passdb:
    - driver: static
      nopassword: true
      fields:
        proxy: "y"
        host: "backend.internal"
```

---

## Password schemes

The `password` column accepts a `{SCHEME}hash` prefix. Without a prefix, the format is autodetected from common crypt(3) markers.

| Scheme | Prefix | Hash format | Notes |
|:---|:---|:---|:---|
| Bcrypt | `{BCRYPT}` / `{BLF-CRYPT}` | `$2a$.../`$2b$.../`$2y$...` | Recommended for new deployments. |
| SHA-512 crypt | `{SHA512-CRYPT}` | `$6$salt$hash` | Linux user import path. |
| Plain | `{PLAIN}` / `{CLEARTEXT}` | literal | **Dev only.** Never store production passwords in plain text. |

Autodetection (no `{SCHEME}` prefix):

| Stored value starts with | Treated as |
|:---|:---|
| `$2a$` / `$2b$` / `$2y$` | BCRYPT |
| `$6$` | SHA512-CRYPT |
| anything else | PLAIN |

### Generating a bcrypt hash

```sh
htpasswd -nbB alice@example.com "topsecret"
# alice@example.com:$2y$05$LhJ...
```

### Generating a SHA-512 crypt hash

```sh
mkpasswd -m sha-512 -S NaClNaCl topsecret
# $6$NaClNaCl$...
```

---

## Adding a user (SQLite example)

```sh
sqlite3 /var/lib/yarilo/users.db <<EOF
INSERT INTO yarilo_users (username, password, enabled)
VALUES (
  'alice@example.com',
  '{BCRYPT}$2y$05$LhJOlnSj4N8u7CC8mvjLeOZjzPGq8GwS9ux/dRrK7uW5UlMnG7r4q',
  1
);
EOF
```

---

## Multiple passdbs

Yarilo tries each entry in order until one returns a result (OK or fail for a known user). Unknown users are passed to the next entry. Useful for: hot-migrating between databases, or shadowing one source with another for testing.

```yaml
auth:
  passdb:
    - driver: sqlite
      dsn: /var/lib/yarilo/legacy.db     # checked first
    - driver: postgres
      dsn: "postgres://...:5432/main"    # falls through to here
```

---

## Custom queries

`password_query`, `user_query`, and `iterate_query` accept any SELECT and can connect yarilo to an existing schema. The query may reference these variables, which are substituted **as parameterised values** (no string interpolation, no injection risk):

| Variable | Meaning | Example |
|:---|:---|:---|
| `%u` | Full username | `alice@example.com` |
| `%n` | Local part (before `@`) | `alice` |
| `%d` | Domain (after `@`) | `example.com` |

> **Do not quote `%u`/`%n`/`%d` in your YAML.** They are rewritten to `?` (sqlite/mysql) or `$1`/`$2`/`$3` (postgres) at runtime. Writing `'%u'` produces literal `'?'` which the DB will treat as a string, not a placeholder.

### Contract

- **`password_query` must return columns in this order:** `password`, `home`, `mail`, `enabled`. Use `AS` aliases to map an existing schema. `password` is the only column whose value is meaningfully used downstream when `user_query` is also set; `home`/`mail` can be empty strings.
- **`user_query` must return:** `home`, `mail`. Called after a successful auth to fill in mailbox location from an authoritative source.
- **`iterate_query` must return one column:** `username`.

### Userdb / passdb extra fields

Beyond `home` / `mail`, a lookup may return extra fields — as a SQL column alias, `user_query` output, or an auth-socket `key=value` / `userdb_*` pair — that tune per-user mailbox, access, and proxying behaviour. Unset fields fall back to the global config. Comma-separated fields (`groups`, `acl_groups`, `quota_rule`, `allow_nets`) accept multiple values and merge across repeated assignments. Boolean fields accept `1` / `yes` / `y` / `true` / `t` / `on` (case-insensitive). Unknown keys are preserved in `Extra`; `forward_*` keys populate the proxy forward map.

**Identity**

| Field | Meaning |
|:---|:---|
| `username` / `user` | Canonical username (overrides the login for master-user flows). |
| `original_user` | The login as typed before normalisation. |
| `master_user` | Master user when the login used master-user syntax. |
| `login_user` | Login user for delegated lookups (equals `username` otherwise). |

**System identity & groups**

| Field | Meaning |
|:---|:---|
| `uid` / `gid` | System user/group id for privilege drop. |
| `home` | User home directory. |
| `chroot` | Chroot directory. |
| `system_groups_user` | Username override for system group lookup. |
| `groups` | Supplementary group names (comma-separated). Matched against `group=` / `group-override=` ACL entries. |
| `client_cert_present` | Bool: TLS client-cert auth was used. |

**ACL evaluation**

| Field | Meaning |
|:---|:---|
| `acl_user` | Override the identity used when **evaluating ACLs** (Dovecot `acl_user`). Typically set on a master-user session so ACL checks resolve as the impersonated user rather than the login. Empty = evaluate as the login user. |
| `acl_groups` | Group names (comma-separated) used alongside `acl_user` for ACL evaluation. |

> `acl_user` / `acl_groups` only affect **non-owner** namespaces (shared / public / other-users). A user always has full rights in their own personal namespace regardless of the override.

**Mail storage**

| Field | Meaning |
|:---|:---|
| `mail` / `mail_location` | Per-user mail location (`maildir:~/Maildir`, `mdbox:…`), with `:INDEX=`, `:CONTROL=`, `:ALT=`, `:VOLATILEDIR=` modifiers. |
| `mail_path` | Base mailbox path (derived from `mail` when unset). |
| `mail_inbox_path` | Explicit INBOX path override. |
| `volatile_dir` / `index_dir` / `control_dir` / `alt_dir` | Direct overrides for the corresponding mail-location modifier (win over modifiers embedded in `mail`). |
| `mail_uid` / `mail_gid` | Ownership for mail files, distinct from the system `uid`/`gid`. |
| `mailbox_format` | `maildir` \| `sdbox` \| `mdbox`. |
| `mail_attribute_dict` | Dict URL backing RFC 5464 METADATA. |

**Quota**

| Field | Meaning |
|:---|:---|
| `quota_rule` | Per-user quota rule, e.g. `*:storage=5G` or `*:messages=100000` (repeatable). |
| `quota_over_flag` | Value marking the user as over quota. |

**Login control**

| Field | Meaning |
|:---|:---|
| `allow_nets` | Allowed source IP/CIDR list (comma-separated); empty = no restriction. |
| `nologin` | Bool: reject login outright. |
| `noauthenticate` | Bool: auth disabled for this user. |
| `nodelay` | Bool: bypass auth-penalty backoff. |
| `pass_expired` | Bool: password expired, client must reset. |
| `nopassword` | Bool: accept any password (passdb). |
| `enabled` | SQL filter column (`WHERE enabled = 1`); not stored as a field. |

**Proxy / director** (see [DIRECTOR.md](DIRECTOR.md))

| Field | Meaning |
|:---|:---|
| `proxy` / `proxy_maybe` | Bool: proxy this session (unconditionally / only if remote). |
| `host` / `port` | Upstream backend address. |
| `destuser` | Username to present to the upstream. |
| `proxy_mech` | SASL mechanism for the upstream login. |
| `proxy_timeout` | Upstream connect timeout (seconds). |
| `proxy_redirect_reauth` | Bool: re-auth on redirect. |
| `proxy_nopipelining` | Bool: disable command pipelining to the upstream. |
| `ssl` / `starttls` | Upstream TLS mode / bool STARTTLS. |

**Connection limits**

| Field | Meaning |
|:---|:---|
| `mail_max_userip_connections` | Max concurrent connections per user+IP. |
| `mail_max_user_connections` | Max concurrent connections per user. |

**Misc**

| Field | Meaning |
|:---|:---|
| `service` | Restrict the entry to a named service. |
| `local_name` | TLS SNI name this entry applies to. |
| `forward_*` | Arbitrary key forwarded to the proxy backend (populates the forward map). |
| *(any other key)* | Preserved verbatim in `Extra`. |

### Example: map an existing schema

```yaml
auth:
  passdb:
    - driver: postgres
      dsn: "postgres://yarilo:${DB_PASSWORD}@db.internal:5432/mailapp"
      skip_schema: true
      password_query: |
        SELECT pw_hash, maildir AS home, mail_path AS mail, active AS enabled
        FROM mailbox_users WHERE email = %u
      user_query: |
        SELECT maildir, mail_path
        FROM mailbox_users WHERE email = %u
      iterate_query: |
        SELECT email FROM mailbox_users WHERE active = 1
      default_pass_scheme: BCRYPT
```

### Example: split passdb across hot/cold sources

```yaml
auth:
  passdb:
    # Fast cache table — recent logins, refreshed by app.
    - driver: postgres
      dsn: "postgres://yarilo:${DB_PASSWORD}@cache.internal:5432/auth"
      skip_schema: true
      password_query: |
        SELECT pw_hash, '/srv/' || %n AS home, '' AS mail, 1 AS enabled
        FROM auth_cache WHERE email = %u

    # Authoritative store — falls through when not in cache.
    - driver: mysql
      dsn: "yarilo:${DB_PASSWORD}@tcp(db.internal:3306)/billing"
      skip_schema: true
      password_query: |
        SELECT password, mail_home AS home, '' AS mail, enabled
        FROM users WHERE email = %u
```

---

## Postfix SASL integration (`auth_service.sasl_listen`)

`yarilo-auth` can expose the Dovecot auth client protocol on a second, plain-TCP listener for Postfix `smtpd_sasl_type=dovecot`. The main listener (`:9100`) uses mTLS and is reserved for yarilo login pods; the SASL listener is plain TCP so Postfix can connect without certificates.

| Key | Default | Description |
|:---|:---|:---|
| `auth_service.sasl_listen` | `""` | Address for the Postfix SASL listener. Empty = disabled. Recommended: `:12345`. |

```yaml
auth_service:
  listen: ":9100"
  sasl_listen: ":12345"
```

Helm:

```yaml
components:
  auth:
    saslListen: ":12345"
```

Postfix `main.cf`:

```
smtpd_sasl_type = dovecot
smtpd_sasl_path = inet:[yarilo-auth.<namespace>.svc]:12345
smtpd_sasl_auth_enable = yes
smtpd_sasl_security_options = noanonymous
```

The Kubernetes Service exposes the configured port on the `yarilo-auth` ClusterIP automatically when `saslListen` is non-empty — no manual service patch needed.

---

## Testing

Unit tests for the SQL passdb cover SQLite end-to-end (via `t.TempDir()`). MySQL and PostgreSQL smoke tests are opt-in via env vars and skipped otherwise:

```sh
YARILO_TEST_MYSQL_DSN="yarilo:secret@tcp(localhost:3306)/yarilo_test?charset=utf8mb4" \
YARILO_TEST_POSTGRES_DSN="postgres://yarilo:secret@localhost:5432/yarilo_test?sslmode=disable" \
go test ./internal/auth/sql/
```

These tests require pre-created empty databases (`yarilo_test`). The schema is auto-created by `New()`.
