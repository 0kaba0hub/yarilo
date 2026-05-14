# Authentication configuration

Yarilo authenticates IMAP/POP3/Submission credentials against an ordered chain of `passdb` entries. The first passdb that returns a definitive result (success or failure for a known user) wins. Unknown users fall through to the next entry.

---

## `auth.passdb`

A list of passdb entries. Each entry has a `driver` and a `dsn`. Order matters — entries are tried left-to-right.

| Key | Description |
|:---|:---|
| `driver` | Backend type: `sqlite` \| `mysql` \| `postgres`. |
| `dsn` | Driver-specific connection string. `${ENV_VAR}` is expanded at startup. |
| `args` | Reserved for driver-specific knobs (not yet used). |

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

## Testing

Unit tests for the SQL passdb cover SQLite end-to-end (via `t.TempDir()`). MySQL and PostgreSQL smoke tests are opt-in via env vars and skipped otherwise:

```sh
YARILO_TEST_MYSQL_DSN="yarilo:secret@tcp(localhost:3306)/yarilo_test?charset=utf8mb4" \
YARILO_TEST_POSTGRES_DSN="postgres://yarilo:secret@localhost:5432/yarilo_test?sslmode=disable" \
go test ./internal/auth/sql/
```

These tests require pre-created empty databases (`yarilo_test`). The schema is auto-created by `New()`.
