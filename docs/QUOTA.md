# Quota (RFC 9208)

yarilo implements per-user storage and message-count quota via the
IMAP QUOTA extension (RFC 9208) and the `pkg/quota` package.

## Architecture

**Limits** come from the userdb `quota_rule=` extra field and flow
through the auth chain into the session at login time:

```
userdb quota_rule=*:storage=5G
  → AuthResponse.QuotaRules
  → userInfo.QuotaRules
  → quota.ParseRules(userInfo.QuotaRules)
  → quota.Limits{StorageBytes: 5*1024^3}
```

**Usage** is tracked as running counters in a configured dict under
two per-user keys:

| Key | Type | Unit |
|:----|:-----|:-----|
| `priv/quota/storage` | int64 string | bytes |
| `priv/quota/messages` | int64 string | message count |

Counters are updated at the session level (not inside the storage
driver) because the session already knows the message size:

- **IMAP APPEND** — quota check before UID allocation; counter
  incremented after successful `AppendMessage`.
- **IMAP EXPUNGE** — counter decremented by `MessageMeta.Size`
  for each expunged message.

## Configuration

Enable quota by declaring a `quota` dict in `yarilo.yaml`:

```yaml
dicts:
  quota:
    driver: redis
    settings:
      addr: yarilo-redis.yarilo.svc.cluster.local:6379
      db: 0
      prefix: "yarilo:quota:"
    expire_secs: 0   # counters do not expire
```

Set a per-user limit in the SQL passdb:

```sql
UPDATE yarilo_users SET quota_rule = '*:storage=5G' WHERE username = 'alice@example.com';
```

The `quota_rule` column can hold a comma-separated list of rules.

### Quota rule format

```
[<mailbox>:]<resource>=<limit>
```

| Example | Meaning |
|:--------|:--------|
| `*:storage=5G` | 5 GiB storage limit (all mailboxes) |
| `*:storage=500M` | 500 MiB limit |
| `*:messages=100000` | 100 000 message limit |
| `*:storage=0` | Unlimited storage |

Units: `K` (KiB), `M` (MiB), `G` (GiB), `T` (TiB). Plain integer
is bytes. `0` means unlimited. Multiple rules are comma-joined in
the SQL column; the last `*:storage=` rule wins.

## IMAP wire (RFC 9208)

When `dicts.quota` is configured the server advertises:

```
* CAPABILITY ... QUOTA
```

Client commands:

```
C: A1 GETQUOTAROOT INBOX
S: * QUOTAROOT INBOX "User quota"
S: * QUOTA "User quota" (STORAGE 1024 5242880 MESSAGE 42 0)
S: A1 OK GETQUOTAROOT completed

C: A2 GETQUOTA "User quota"
S: * QUOTA "User quota" (STORAGE 1024 5242880 MESSAGE 42 0)
S: A2 OK GETQUOTA completed

C: A3 SETQUOTA "User quota" (STORAGE 10485760)
S: A3 NO [NOPERM] Permission denied
```

`STORAGE` values are in kibibytes (1 KiB = 1024 bytes). `MESSAGE`
values are raw counts. Limit `0` means unlimited.

`SETQUOTA` is always rejected — limits are operator-managed only.

## Enforcement

When a user is over quota, `APPEND` returns:

```
NO [OVERQUOTA] Quota exceeded
```

The check is a pre-APPEND read of current usage from the dict.
On dict error the check is skipped (fail-open) so a transient dict
outage does not prevent mail delivery.

## Admin API

```sh
# Show current usage
yarilo-admin backend quota show alice@example.com

# Rebuild counters from disk (after manual migration or drift)
yarilo-admin backend quota recalc alice@example.com

# Directly set counter values
yarilo-admin backend quota set alice@example.com --storage-bytes 1073741824 --messages 100
```

These call `POST /api/backend/quota/recalc` and related endpoints
on `yarilo-backend-api`.

### Recalc

`recalc` walks all folder fileindexes, sums `MessageMeta.Size` and
overwrites the dict counters. Run after:

- Manual mailbox migration
- Counter drift (caused by crashes between Save and counter update)
- Enabling quota on an existing deployment

```sh
# k8s CronJob — weekly recalc for all users
# Loop over users from yarilo-admin backend user iterate
```

## Helm

```yaml
# values.yaml
dicts:
  quota:
    driver: redis
    settings:
      addr: "yarilo-redis.{{ .Release.Namespace }}.svc.cluster.local:6379"
      db: 0
      prefix: "yarilo:quota:"
    expire_secs: 0
```

Mount a Redis instance (or any supported dict driver) and set
`quota_rule` in the passdb schema.
