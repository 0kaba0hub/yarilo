# Dict — yarilo's key-value abstraction

`pkg/dict` is the general key-value store every yarilo feature that needs
durable per-user or per-mailbox state sits on top of. A single contract
(`Dict` + `Tx` + `Iterator`) is satisfied by multiple drivers; the
choice of driver (`file`, `redis`, `sql`, `memory`, `fail`) is made via
YAML config, not code.

See [ARCHITECTURE.md §Dict abstraction](../ARCHITECTURE.md#dict-abstraction)
for the design rationale; this document is the operator reference for
configuring and operating dicts.

---

## Concepts

| Term | Meaning |
|:---|:---|
| **Dict** | A named key-value store instance, declared once in `yarilo.yaml` under `dicts:` |
| **Driver** | The backing implementation (`file`, `redis`, `sql`, `memory`, `fail`) |
| **Settings** | Driver-specific configuration map (path / addr / dsn / ...) |
| **Namespace** | `priv/` (per-user) and `shared/` (per-resource) key prefixes — application convention; not enforced by drivers |
| **OpSettings** | Per-call context: username, home dir, TTL — passed by callers, used by drivers |

## YAML schema

```yaml
dicts:
  <name>:
    driver: file|redis|sql|memory|fail
    settings:
      # driver-specific (see below)
    expire_secs: 0          # default TTL for writes (drivers with TTL support)
    username: ""            # default OpSettings.Username
    home_dir: ""            # default OpSettings.HomeDir
```

`<name>` is the logical identifier yarilo features look up
(e.g. `metadata`, `quota_count`, `acl`). Multiple named dicts may share
one backing service through different prefixes/namespaces.

---

## Drivers

### file

JSON file on local disk. Atomic temp-file + rename on every commit.
In-process sync.RWMutex; NOT safe across processes. Use for standalone
single-pod deployments, dev runs, smoke tests.

```yaml
dicts:
  metadata:
    driver: file
    settings:
      path: "/var/yarilo/dicts/metadata.json"
```

Settings:

| Key | Type | Required | Meaning |
|:---|:---|:---|:---|
| `path` | string | yes | Filesystem path; expand `%u`/`%h`/`%n`/`%d` *before* opening |

### memory

In-process `map[string]row`. Lost on process exit. For unit tests and
short-lived dev runs.

```yaml
dicts:
  scratch:
    driver: memory
```

No settings.

### fail

Every operation returns `ErrFailDriver`. Used in code paths that need a
non-nil `dict.Dict` even when the feature is disabled.

```yaml
dicts:
  disabled-metadata:
    driver: fail
    settings:
      message: "metadata feature disabled by admin"   # optional
```

### redis

Production cluster backend. SET/GET/DEL, MULTI/EXEC for transactions,
INCRBY for atomic counters, EXPIRE for TTL, SCAN for iteration. One
Redis string per dict key. Prefix-isolated so multiple named dicts can
share one Redis instance.

```yaml
dicts:
  metadata:
    driver: redis
    settings:
      addr: "yarilo-redis.yarilo.svc.cluster.local:6379"
      password: ""                # optional AUTH
      db: 0                       # logical database
      prefix: "yarilo:metadata:"  # prepended to every key on the wire
      dial_timeout: "5s"          # Go duration string
    expire_secs: 86400            # default TTL: 1 day
```

Settings:

| Key | Type | Required | Default | Meaning |
|:---|:---|:---|:---|:---|
| `addr` | string | yes | — | `host:port` |
| `password` | string | no | `""` | AUTH; empty = no auth |
| `db` | int | no | `0` | Logical database |
| `prefix` | string | no | `""` | Per-dict key prefix |
| `dial_timeout` | string | no | `5s` | Go duration |

### sql

Production cluster backend. PostgreSQL, MySQL or SQLite via `database/sql`.
One row per dict key. Auto-creates the table on first Open. Per-namespace
to allow multiple dicts in one schema. See **Mapped mode** below for a
column-per-key layout (quota_clone).

```yaml
dicts:
  metadata:
    driver: sql
    settings:
      driver: postgres            # | sqlite
      dsn: "postgres://yarilo:secret@pg.yarilo.svc:5432/yarilo?sslmode=disable"
      table: "dict_kv"            # default
      namespace: "metadata"       # per-dict namespace within the table
```

Settings:

| Key | Type | Required | Default | Meaning |
|:---|:---|:---|:---|:---|
| `driver` | string | yes | — | `sqlite`, `postgres` or `mysql` |
| `dsn` | string | yes | — | `database/sql` DSN |
| `table` | string | no | `dict_kv` | Table name; must match `[A-Za-z0-9_]+` (generic mode) |
| `namespace` | string | no | `""` | Per-dict key prefix within the shared table (generic mode) |
| `maps` | list | no | — | Column bindings; presence enables **mapped mode** (see below) |

Schema (auto-created):

```sql
CREATE TABLE dict_kv (
    namespace TEXT NOT NULL,
    k         TEXT NOT NULL,
    v         BLOB NOT NULL,         -- BYTEA on postgres
    expires   BIGINT,                -- unix seconds; NULL = no TTL
    PRIMARY KEY (namespace, k)
);
CREATE INDEX dict_kv_expires_idx ON dict_kv(expires) WHERE expires IS NOT NULL;
```

`ExpireScan` runs `DELETE FROM dict_kv WHERE namespace = $1 AND expires <= $2`.

#### Mapped mode (column mapping)

By default the sql driver stores every key in the generic `(namespace, k, v)`
layout, so a quota_clone target writes two rows per user. Setting `maps` switches
the dict to **mapped mode**: each key binds to a table **column**, producing a
clean per-user schema an external reader (billing, dashboards) can query
directly. Keys mapped to the same table share one row (the `username_field` is
the primary key), so different columns of the same user coexist.

```yaml
dicts:
  quota_clone_mysql:
    driver: sql
    settings:
      driver: mysql               # sqlite | postgres | mysql
      dsn: "${YARILO_DB_DSN}"
      maps:
        - { key: "priv/quota/storage",  table: quota, username_field: username, value_field: bytes }
        - { key: "priv/quota/messages", table: quota, username_field: username, value_field: messages }
```

The operator owns the table — mapped mode does **not** auto-create it (column
types are the operator's choice):

```sql
CREATE TABLE quota (username VARCHAR(255) PRIMARY KEY, bytes BIGINT, messages BIGINT);
-- one row per user: (u1@d00001.test, 860809, 13)
```

> **Every column other than `username_field` must be nullable (or carry a
> `DEFAULT`).** A `Set` inserts only `(username_field, value_field)`, so the
> first write for a new user leaves sibling columns unset — a `NOT NULL` sibling
> would reject that insert. `Unset` also relies on the column being nullable.

Mapped mode is validated at startup: `New` runs a `SELECT <value_field> FROM
<table> LIMIT 0` for every map, so a typo in a table/column name or a forgotten
`CREATE TABLE` fails at Open rather than silently on the first write. Per-key
TTL (`expire_secs`) and `ExpireScan` are unavailable in mapped mode (mapped
columns carry no expiry) and return an error.

Each map entry:

| Field | Required | Meaning |
|:---|:---|:---|
| `key` | yes | Dict key to map (matched exactly) |
| `table` | yes | Target table; must match `[A-Za-z0-9_]+` |
| `username_field` | yes | Column holding the user (primary key); scoped by `OpSettings.Username` |
| `value_field` | yes | Column the key's value is written to / read from |

Behaviour in mapped mode:

- **Set** → single-column upsert keyed on `username_field` (`ON DUPLICATE KEY UPDATE` / `ON CONFLICT DO UPDATE`).
- **Lookup** → `SELECT <value_field> FROM <table> WHERE <username_field> = ?`; a `NULL` column reads as "not found".
- **Unset** → sets the column to `NULL` (the row is shared by sibling columns and is never deleted).
- **AtomicInc** / **Iterate** → unsupported (error); quota_clone uses only Set.
- A username is required; an unmapped key errors.

---

## CLI — `yarctl dict`

### Select the dict

Either via running config:

```sh
yarctl dict <command> --config /etc/yarilo.yaml --dict metadata ...
```

Or ad-hoc (no config required):

```sh
yarctl dict <command> --driver file --setting path=/tmp/x.dict ...
yarctl dict <command> --driver redis --setting addr=localhost:6379 --setting prefix=test: ...
```

Per-op identity:

| Flag | Maps to |
|:---|:---|
| `--user USER` | `OpSettings.Username` |
| `--home DIR` | `OpSettings.HomeDir` |
| `--expire-secs N` | `OpSettings.ExpireSecs` |

### Commands

```sh
yarctl dict drivers                                                # list registered driver names

yarctl dict lookup [select] KEY                                    # print value
yarctl dict iterate [select] [--recurse] [--no-value] [--exact] \
                          [--sort-key|--sort-value] PATH                 # list rows

yarctl dict set    [select] [--value-stdin] KEY [VALUE]            # write
yarctl dict unset  [select] KEY                                    # delete
yarctl dict atomic-inc [select] KEY DELTA                          # integer add (delta may be negative)

yarctl dict expire-scan [select]                                   # drop TTL-expired rows

yarctl dict commit-batch [select] < script.txt                     # multi-op atomic transaction
```

### `commit-batch` script format

TAB-delimited, one op per line. Empty lines and `#`-prefixed lines are
ignored. Values are base64-encoded so binary content survives.

```
# Initialise per-user quota counters
set	priv/quota/storage	MA==
set	priv/quota/messages	MA==

# Bump storage by 1 KiB (delta is plain text, not base64)
atomic-inc	priv/quota/storage	1024

unset	priv/old/key
```

Pipe the script:

```sh
yarctl dict commit-batch --config /etc/yarilo.yaml --dict quota < initialise.dict
```

### Example session

```sh
# Standalone development dict via file driver
$ yarctl dict set --driver file --setting path=/tmp/m.dict priv/box/INBOX/comment "first message arrived"
ok

$ yarctl dict lookup --driver file --setting path=/tmp/m.dict priv/box/INBOX/comment
first message arrived

$ yarctl dict iterate --driver file --setting path=/tmp/m.dict --recurse --sort-key priv/
priv/box/INBOX/comment	first message arrived
```

---

## Choosing a driver

| Topology | Recommended driver |
|:---|:---|
| Standalone single-pod helm release | `file` (mounted on the pod's PVC) |
| Backend multi-pod helm release | `redis` (shared Redis Service) |
| Already running Postgres for other yarilo state | `sql` driver, `postgres` mode |
| Unit tests | `memory` |
| "Feature disabled" wiring | `fail` |

The choice is config-only — switching from `file` (standalone) to `redis`
(backend) is a `yarilo.yaml` edit, not a rebuild. This is the
**config-not-binary** rule that every yarilo storage decision honours.
