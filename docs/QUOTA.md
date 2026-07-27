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

## quota_clone (external mirror)

`quota_clone` mirrors the current usage into external dicts for **external**
consumers outside the mail server (provisioning DB, dashboard). It is **not**
part of enforcement — enforcement always reads the index; yarilo's own
quota-status opens the mailbox. The mirror is advisory and never the source of
truth.

The trigger matches the reference plugin: on a usage-changing event (save /
copy / expunge) the session marks the mirror dirty and, at most once per
`quota_clone_flush_delay` seconds, reads the authoritative count and writes it
out; a final flush runs on session close. Only **active** users (with an open
session) are mirrored — there is no full-user sweep.

yarilo's enhancement over the single-dict reference is a **multi-dict fan-out**:
several targets are written in parallel (e.g. SQL + Redis at once), each
best-effort — a failing target is logged and never blocks the others or the
authoritative path.

```yaml
quota:
  quota_clone_dicts: [quota_clone_sql, quota_clone_redis]  # names from dicts:
  quota_clone_flush_delay: 10
dicts:
  quota_clone_sql:   { driver: sql,   ... }
  quota_clone_redis: { driver: redis, ... }
```

Keys written per user: `priv/quota/storage` (bytes) and `priv/quota/messages`
(count) — the same layout Dovecot's clone uses, so existing external readers
work unchanged.

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
| `quota_hidden` | `false` | Omit the quota root from GETQUOTA/GETQUOTAROOT for **every** user (enforcement still applies). Broader than `quota_ignore_unlimited`. |

Effective storage limit = `rule_limit · quota_storage_percentage/100 + quota_storage_extra`
(`+ quota_grace` on LMTP delivery). The scaled limit is what GETQUOTA reports and
what every enforcement point checks.

### Quota warnings

`quota_warning` runs an action when a user's usage **crosses** a percentage of
their limit — the same edge-trigger as the reference `quota_warning_match`
(fires once on the transition, not repeatedly while over). The action is a
program in `quota_warning_bin_dir` (mirrors `sieve_execute_bin_dir`), run
best-effort; when no bin dir is set the crossing is only logged.

```yaml
quota:
  quota_warning_bin_dir: "/usr/lib/yarilo/quota-warning"
  quota_warning_exec_timeout: 10
  quota_warnings:
    - quota_warning_name: "storage90"
      quota_warning_resource: storage      # storage | message
      quota_warning_threshold: over        # over | under
      quota_warning_percentage: 90         # % of the user's limit
      quota_warning_execute: "warn-user"   # program name in the bin dir
```

The program is located by bare name inside the bin dir (a path separator is
rejected) and receives the crossing context via the environment: `USER`,
`HOME`, `HOST`, `QUOTA_WARNING_NAME`, `QUOTA_RESOURCE`, `QUOTA_THRESHOLD`,
`QUOTA_PERCENTAGE`, `QUOTA_USAGE`, `QUOTA_LIMIT`.

`over` crossings fire on save (IMAP APPEND/COPY/MOVE, LMTP delivery); `under`
crossings fire on the IMAP expunge that drops usage back below the threshold.

## quota_over_status — external over-flag sync

Keeps an external "over quota" flag in sync so an MTA can reject mail to
over-quota users without querying the mail server. At **login** (IMAP) the actual
over-quota state is compared to the userdb `quota_over_flag`; on a mismatch a
program updates the external flag.

```yaml
quota:
  quota_warning_bin_dir: "/usr/lib/yarilo/quota-warning"   # shared with warnings
  quota_over_status_mask: "TRUE"          # wildcard the userdb flag is matched against
  quota_over_status_lazy_check: false     # true = defer to the first quota op
  quota_over_status_execute: "sync-flag"  # program in the bin dir
```

- `quota_over_flag` (userdb) is the stored flag. `flagged` = it is non-empty and
  matches `quota_over_status_mask` (case-insensitive `*`/`?` glob).
- `actual` = usage ≥ limit for any resource.
- On `actual != flagged` the program runs with `USER`, `HOME`, `HOST`,
  `QUOTA_OVER_FLAG` (the stale value), `QUOTA_OVER` (`yes`/`no`) in the
  environment. The check runs once per session and only while the userdb flag is
  still fresh (login within 10 s).
- Empty `quota_over_status_mask` disables it. Currently wired for IMAP logins
  (POP3 lacks the usage-count path — a separate task).

## quota_status_nouser

The `yarilo-quota-status` policy service returns `quota_status_nouser` when the
recipient is unknown in userdb (default `REJECT Unknown user`). A backend lookup
**error** still fails open (`DUNNO`); set `quota_status_nouser: ""` to accept
unknown recipients (`DUNNO`) and let a later Postfix restriction decide.

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
yarctl backend quota show alice@example.com

# Force-rebuild each folder's aggregate from records, then report
yarctl backend quota recalc alice@example.com
```

These call `GET /api/backend/quota/show` and `POST /api/backend/quota/recalc`
on `yarilo-backend-api`. There is no `set` — usage is computed, not stored.

### Recalc

The index self-heals on load (the O(1) validity check), so `recalc` is only
needed to force a rebuild after suspected aggregate corruption. It reopens each
folder, recomputes `hdr-vsize` from the per-record vsize, and returns the sum.

### Inspecting the quota_clone mirror

`quota_clone` mirrors the authoritative usage into one or more external dicts
(SQL + Redis fan-out). To confirm the fan-out is coherent or debug a divergent
target:

```sh
# List the configured clone backends
yarctl backend quota clone list

# Read what one backend holds for a mailbox (advisory mirror)
yarctl backend quota clone get quota_clone_mysql alice@example.com
```

These call `GET /api/backend/quota/clone/list` and
`GET /api/backend/quota/clone/get?backend=<name>&user=<user>`, which reads
`priv/quota/storage` + `priv/quota/messages` from that dict, scoped per user.
`backend` is restricted to the configured clone list — for arbitrary dicts use
`yarctl backend dict get`. The returned value is an **advisory mirror**;
the authoritative usage is `quota show`, summed from the index.

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
