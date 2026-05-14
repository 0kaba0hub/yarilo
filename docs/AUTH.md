# Authentication configuration

Yarilo authenticates IMAP/POP3/Submission credentials against an ordered chain of `passdb` entries. The first passdb that returns a definitive result (success or failure for a known user) wins. Unknown users fall through to the next entry.

---

## `auth.passdb`

A list of passdb entries. Each entry has a `driver` and a `dsn`. Order matters — entries are tried left-to-right.

| Key | Description |
|:---|:---|
| `driver` | Backend type: `sqlite` \| `mysql` \| `postgres`. |
| `dsn` | Driver-specific connection string. `${ENV_VAR}` is expanded at startup. |
| `password_query` | Optional custom SELECT for authentication. Defaults to the built-in `yarilo_users` schema. See [Custom queries](#custom-queries). |
| `user_query` | Optional separate userdb lookup (`home`, `mail`). When unset, userdb fields come from `password_query`. |
| `iterate_query` | Optional list-users query for admin tooling. |
| `default_pass_scheme` | Assumed scheme when stored password has no `{SCHEME}` prefix and no crypt(3) marker. Default: `PLAIN`. |
| `skip_schema` | `true` to skip `CREATE TABLE IF NOT EXISTS yarilo_users` on startup — use when connecting to an existing schema. |

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

## Testing

Unit tests for the SQL passdb cover SQLite end-to-end (via `t.TempDir()`). MySQL and PostgreSQL smoke tests are opt-in via env vars and skipped otherwise:

```sh
YARILO_TEST_MYSQL_DSN="yarilo:secret@tcp(localhost:3306)/yarilo_test?charset=utf8mb4" \
YARILO_TEST_POSTGRES_DSN="postgres://yarilo:secret@localhost:5432/yarilo_test?sslmode=disable" \
go test ./internal/auth/sql/
```

These tests require pre-created empty databases (`yarilo_test`). The schema is auto-created by `New()`.
