# Quota (RFC 9208)

yarilo implements per-user storage and message-count quota via the
IMAP QUOTA extension (RFC 9208) and the `pkg/quota` package.

## Architecture

**Limits** come from the userdb `quota_rule=` extra field:

```
userdb quota_rule=*:storage=5G → AuthResponse.QuotaRules → userInfo.QuotaRules
  → quota.ParseRules(...) → quota.Limits{StorageBytes: 5*1024^3}
```

**Usage is the `count` backend — derived from the index, never a stored
counter.** This mirrors Dovecot 2.4, which *removed* its `dict` quota backend
(the drift-prone "counter is the source of truth" model); the authoritative
value is computed from the mailbox index.

- The FileIndex carries two extensions (see INTERNALS.md): a per-record `vsize`
  (virtual/RFC822 size) and a header `hdr-vsize` aggregate
  `{vsize, highest_uid, message_count}`.
- The aggregate is maintained incrementally on append/expunge and **self-heals**:
  on load it is trusted only while `highest_uid+1 == uidnext && message_count ==
  messages` (a cheap O(1) validity check, exactly Dovecot's), otherwise it is
  recomputed from the per-record vsize.
- `quota.CountUsage(idx, folders, limits)` sums each folder's `FolderVSize`
  aggregate — the authoritative usage. `ignore` folders are skipped.

**Every enforcer reads from the index** (no dict in the enforcement path):

| Path | How usage is read |
|:---|:---|
| IMAP GETQUOTA / APPEND enforcement | `session.countUsage` → sum `FolderVSize` (1 s display cache; enforcement fresh) |
| LMTP delivery | opens the recipient index at delivery time → `CountUsage` |
| `yarilo-quota-status` (Postfix policy) | a **full mail process**: opens the recipient's mailbox+index → `CountUsage`, exactly like Dovecot's quota-status (`mail_storage_service`). **Not** a dict reader. Needs the mail PV mounted. |
| `backend-api` /show, /recalc | `CountUsage`; /recalc force-rebuilds each folder's aggregate |
| POP3 | **nothing** — POP3 never appends, so no enforcement; DELE→expunge decrements the index aggregate automatically |

## Two distinct entities — do not conflate

1. **quota engine** (`quota.enabled`) — the count backend + enforcement on
   **every save**: IMAP APPEND/COPY/MOVE (OVERQUOTA), LMTP delivery (452), and
   the quota-status policy service. This mirrors Dovecot's `quota` plugin, whose
   `quota-storage.c` hooks `mail_save` — the path shared by APPEND and delivery.
2. **IMAP QUOTA extension** (`protocol.imap.imap_quota`) — RFC 9208
   `GETQUOTA` / `GETQUOTAROOT` + the `QUOTA` capability. A **client-facing query
   only, no enforcement** (Dovecot's `imap_quota` plugin registers only the
   commands). Toggle it independently of the engine — you can enforce without
   advertising the extension, or advertise it without enforcing.

Both default off in Helm (`quota.enabled: false`, `protocol.imap.imap_quota: true`).

## quota_clone (external mirror) — separate, optional, not yet built

Dovecot's `quota_clone` mirrors the current usage into a dict for **external**
consumers outside the mail server (provisioning DB, dashboard). It is **not**
part of enforcement (Dovecot's own quota-status opens the mailbox). yarilo will
add it as an optional feature with a **multi-dict fan-out** (write to SQL + Redis
in parallel — Dovecot allows only one dict). The `dicts.quota` config is reserved
for this; nothing reads it in the enforcement path.

## TODO (tracked in #549)

- **quota_grace** — bounded temporary overshoot (Dovecot `quota_storage_grace`),
  threaded into `IsOver`.
- **quota_warning** — threshold events (80 %/95 %) via the locks bus / webhook /
  Sieve notification.
- **quota_clone** — the multi-dict external mirror above.

## Configuration

Two independent toggles (both default off/on per Helm):

```yaml
quota:
  enabled: true                    # engine: enforce on every save (APPEND/COPY/MOVE, LMTP, quota-status)
  quota_name: "User quota"               # quota-root name in GETQUOTA / GETQUOTAROOT
  quota_exceeded_message: "Quota exceeded (mailbox for user is full)"  # over-quota rejection text
  quota_mail_size: ""                    # reject any single message larger than this ("50M"); ""/"0" = unlimited
protocol:
  imap:
    imap_quota: true   # IMAP QUOTA extension: advertise QUOTA + answer GETQUOTA (query only)
```

`quota_mail_size` is independent of the usage limit and applies even without a
per-user `quota_rule`; its rejection carries a distinct "exceeds max mail size"
text so a client can tell "message too large" from "mailbox full". The
`quota-status` policy service additionally honours `quota_status.recipient_delimiter`
(default `+`) when deriving the target folder from the recipient detail part.

No dict is needed — usage is summed from the index. Set a per-user limit in the SQL passdb:

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

### Site-wide policy options

These are global `quota:` config (not per-user rules) and layer on top of the
resolved per-user limits:

| Key | Default | Effect |
|:----|:--------|:-------|
| `quota_storage_percentage` | `100` | Scale the storage limit: `limit·pct/100`. |
| `quota_message_percentage` | `100` | Scale the message-count limit. |
| `quota_storage_extra` | `` | Byte headroom added to the storage limit after scaling. |
| `quota_grace` | `10M` | Storage overshoot allowed on **inbound delivery (LMTP/LDA) only** — never interactive IMAP. Lets a nearly-full mailbox accept one more delivery. |
| `quota_ignore_unlimited` | `false` | Omit the quota root from GETQUOTA/GETQUOTAROOT for unlimited users. |
| `quota_mailbox_count` | `0` | Cap the number of mailboxes (folders). Enforced at CREATE — `NO [LIMIT] Maximum number of mailboxes reached`. `0` = unlimited. |
| `quota_mailbox_message_count` | `0` | Cap messages in a single mailbox. Enforced on save — `NO [OVERQUOTA] Too many messages in the mailbox` (LMTP `552`). `0` = unlimited. |

Effective storage limit = `rule_limit · quota_storage_percentage/100 + quota_storage_extra`
(`+ quota_grace` on LMTP delivery). The scaled limit is what GETQUOTA reports and
what every enforcement point checks.

## IMAP wire (RFC 9208)

When `protocol.imap.imap_quota` is on the server advertises:

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

When a user is over quota, `APPEND` returns (text from `quota.quota_exceeded_message`):

```
NO [OVERQUOTA] Quota exceeded (mailbox for user is full)
```

A message larger than `quota.quota_mail_size` is rejected regardless of usage with a
distinct text:

```
NO [OVERQUOTA] Requested allocation size 1200000 exceeds max mail size 1048576
```

The check reads current usage from the index. On a transient read error
it is skipped (fail-open) so an I/O blip does not block delivery.

## Admin API

```sh
# Show current usage (summed from the index)
yarilo-admin backend quota show alice@example.com

# Force-rebuild each folder's aggregate from records, then report
yarilo-admin backend quota recalc alice@example.com
```

These call `GET /api/backend/quota/show` and `POST /api/backend/quota/recalc`
on `yarilo-backend-api`. There is no `set` — usage is computed, not stored.

### Recalc

The index self-heals on load (the O(1) validity check), so `recalc` is only
needed to force a rebuild after suspected aggregate corruption. It reopens each
folder, recomputes `hdr-vsize` from the per-record vsize, and returns the sum.

## Helm

```yaml
# values.yaml
quota:
  enabled: true        # enforce
protocol:
  imap:
    imap_quota: true   # advertise the QUOTA extension
```

The `yarilo-quota-status` pod mounts the mail PV read-only so it can open
recipient mailboxes; set `quota_rule` in the passdb schema.
