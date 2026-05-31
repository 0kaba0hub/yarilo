# Namespaces — IMAP NAMESPACE (RFC 2342 / RFC 9051 §6.3.10)

yarilo supports the three RFC 9051 namespace classes:

| Class | Typical prefix | Purpose |
|:---|:---|:---|
| **Personal** | `""` | The user's own mailboxes (INBOX + everything created via CREATE). |
| **Other Users** | `user/` | Read/write access to another user's mailboxes, gated by ACL. |
| **Shared** | `Shared/` | Folders shared between groups of users (or all users), gated by ACL. |
| **Public** (a variant of Shared) | `Public/` | Folders accessible to every authenticated user. |

This page is the operator reference for **NS-1a** (wire-protocol — `v1.20`).
NS-1b (storage routing — `OpenNamespace` per-namespace backends) and
ACL-1 (RFC 4314 access control) ship in subsequent releases. Until those
land, only Personal carries real mailboxes — Other / Shared / Public can
be **declared** in config and will appear in the IMAP `NAMESPACE`
response, but `SELECT`-ing a mailbox under them returns `NO`.

---

## YAML schema

```yaml
namespaces:
  - type: personal              # required: personal | other | shared
    prefix: ""                  # mailbox name prefix; "" reserved for personal
    separator: "/"              # one character; different per-namespace allowed
    list: true                  # show in NAMESPACE response
    subscriptions: true         # track SUBSCRIBE state for this namespace
    inbox: true                 # owns the magic "INBOX" mailbox (set on exactly one)
    location: "maildir:%h"      # NS-1b: storage URL; varexpand %u/%h/%n/%d/%i
    hidden: false               # NS-1b: hide matching mailboxes from LIST "" "*"
```

### Default (when `namespaces:` is omitted)

```yaml
namespaces:
  - type: personal
    prefix: ""
    separator: "/"
    list: true
```

Equivalent to pre-v1.20 behaviour — the IMAP `NAMESPACE` response is
`* NAMESPACE (("" "/")) NIL NIL`.

### Personal + Shared + Other Users (Dovecot-style)

```yaml
namespaces:
  - type: personal
    prefix: ""
    separator: "/"
    list: true
    inbox: true
    location: "maildir:%h"

  - type: shared
    prefix: "Shared/"
    separator: "/"
    list: true
    location: "maildir:/var/yarilo/shared"

  - type: other                # Dovecot's "Other Users" namespace
    prefix: "user/"            # so client sees "user/alice/INBOX"
    separator: "/"
    list: true
    location: "maildir:%shared_root/%n"   # NS-1b: per-owner directory
```

Wire shape (post-AUTHENTICATE):

```
C: A1 NAMESPACE
S: * NAMESPACE (("" "/")) (("user/" "/")) (("Shared/" "/"))
S: A1 OK NAMESPACE completed
```

---

## Per-namespace separator

yarilo follows Dovecot: each namespace MAY use a different separator.

| Field | Constraint |
|:---|:---|
| `separator` | exactly one character. Missing → defaults to `/`. Multi-char → falls back to `/` with a warning at startup. |

Useful when migrating from a legacy Dovecot deployment that used `.` for
personal mailboxes (mbox legacy) and `/` for shared:

```yaml
namespaces:
  - type: personal
    prefix: ""
    separator: "."
    list: true
  - type: shared
    prefix: "Shared/"
    separator: "/"
    list: true
```

---

## Quota interaction (NS-1b + QUOTA-1)

Quota is **owner-paid**: storage consumed in `user/alice/INBOX` counts
against alice's quota, not against the user accessing it. Public/Shared
namespaces have their own system-wide quota root (configured in the
`quota:` block, not here). See [QUOTA.md](QUOTA.md) when QUOTA-1 lands.

---

## Hidden namespaces (`list: false`)

`list: false` keeps a namespace addressable internally (NS-1b storage
routing respects it) without advertising it in the `NAMESPACE` response.
Used for staging — declare and configure backends for a shared
namespace, smoke-test access from privileged accounts, then flip `list`
to `true` to expose it to all users.

---

## What does NOT work yet (post-NS-1a)

| Behaviour | Phase that delivers it |
|:---|:---|
| `SELECT Shared/marketing/announcements` actually opens a mailbox on the shared backend | NS-1b |
| `LIST "" "*"` returns mailboxes from across all configured namespaces | NS-1b |
| Per-namespace storage backend (separate maildir root, possibly a different driver) | NS-1b |
| `LIST` and `SELECT` enforce per-folder rights | ACL-1 (RFC 4314) |
| `GETMETADATA` priv/ on a shared mailbox is per-accessing-user | NS-1b |
| Quota debit on writes to `user/alice/*` charges alice | QUOTA-1 + NS-1b |
| Director routes `user/alice/*` to alice's backend pod | NS-3 |

Until then, declaring Shared/Other namespaces produces a correct
`NAMESPACE` response but `SELECT`-ing under them returns
`NO "No such mailbox"`. Operators who want to stage their full
namespace topology can do it now — when NS-1b ships, no config change
is needed beyond filling in `location:` per namespace.
