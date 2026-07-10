# Namespaces — IMAP NAMESPACE (RFC 2342 / RFC 9051 §6.3.10)

yarilo supports the three RFC 9051 namespace classes:

| Class | Typical prefix | Purpose |
|:---|:---|:---|
| **Personal** | `""` | The user's own mailboxes (INBOX + everything created via CREATE). |
| **Other Users** | `user/` | Read/write access to another user's mailboxes, gated by ACL. |
| **Shared** | `Shared/` | Folders shared between groups of users (or all users), gated by ACL. |
| **Public** (a variant of Shared) | `Public/` | Folders accessible to every authenticated user. |

This page covers **NS-1a** (wire-protocol, `v1.20`) + **NS-1b**
(storage routing, `v1.21`). With NS-1b shipped, **Personal and
Shared/Public** mailboxes carry real storage; **Other Users**
(`user/<owner>/...`) is declared but `SELECT` under that prefix
returns `NO "Other Users namespace requires ACL-1 + NS-3"`.

ACL-1 (RFC 4314 access control) lands next — before that, **any
authenticated user reads and writes everything under Shared/Public**.
Treat shared/public namespaces as cooperative-trust until ACL-1 lands.

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
    prefix: "user/%u/"         # %u ⇒ owner-templated (client sees "user/alice/INBOX")
    separator: "/"
    list: true
    location: "maildir:%h"     # %h/%u/%n/%d expand against the OWNER (alice)
```

### Owner-templated namespaces (NS-2, designed)

When a namespace `prefix` contains an owner variable (`%u`, `%n`, or `%d` — e.g.
`user/%u/`), the namespace is **owner-templated**: the `%u/%n/%d/%h` variables
in its `location` expand against the **owner** whose name fills the prefix slot,
not the logged-in user. Accessing `user/alice/Sent` extracts `owner = alice`,
looks alice up in the userdb, and resolves `location` against alice's storage.
The owner's own session has implicit full rights; a peer is gated by the
owner's ACL. Fixed prefixes (no variable, e.g. `Shared/`, `Public/`) are
unaffected and resolve to one path for everyone.

This is **single-pod / standalone** in item 3 (#499); cross-pod owner storage
is NS-3. See [OWNER_SHARED_NS.md](OWNER_SHARED_NS.md) for the full design.

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

## Storage layout

Each namespace's storage is rooted at its `location:`. The operator
mounts (or pre-creates) the path; yarilo creates the per-folder
maildir tree on the first `CREATE` / `APPEND`.

```
/var/mail/vhosts/<domain>/<user>/          ← personal (per-user, existing layout)
  Maildir/
    .index
    cur/  new/  tmp/
    .Sent/...

/var/yarilo/shared/                        ← shared (one root per install)
  marketing/
    announcements/
      .index
      cur/  new/

/var/yarilo/public/                        ← public (one root per install)
  announcements/...
```

For multi-pod backend deployments the namespace roots must be on
shared storage (NFS/CephFS RWX) so all replicas see the same shared
folder tree. The standalone helm chart leaves shared roots **empty by
default** — operators opt in by populating `cfg.Namespaces` and
mounting a PV at the chosen `location:`.

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

## What works in NS-1b (`v1.21`)

| Behaviour | Status |
|:---|:---|
| `SELECT Shared/marketing/announcements` opens a mailbox on the shared backend | ✅ |
| `CREATE Shared/team` lands under the configured `location:` (separate filesystem root) | ✅ |
| `APPEND` / `FETCH` / `STORE` / `EXPUNGE` / `SEARCH` on shared mailboxes | ✅ |
| `LIST "" "*"` returns mailboxes from every configured namespace, each row prefixed with its namespace prefix and emitting its own separator | ✅ |
| `SUBSCRIBE Shared/team` persists to a per-namespace subscription file (`subscriptions-shared`) — separate from `subscriptions` (personal) | ✅ |
| `COPY` / `MOVE` between personal and shared namespaces | ✅ |
| `RENAME` within a single namespace | ✅ |
| `GETMETADATA` / `SETMETADATA` on shared folders, with `/private/*` stored **per accessing user** (SHA-256 hash of username), `/shared/*` global to the folder | ✅ |
| `SELECT user/alice/INBOX` (Other Users) | `NO "Other Users namespace requires ACL-1 + NS-3"` |
| `LIST` of `user/*` patterns | returns empty (namespace declared but unimplemented) |

## What does NOT work yet (post-NS-1b)

| Behaviour | Phase that delivers it |
|:---|:---|
| `LIST` / `SELECT` enforce per-folder rights (anyone with creds reads/writes Shared/Public) | ACL-1 (RFC 4314) |
| `RENAME` across namespaces (`Personal/foo` → `Shared/foo`) | declined with `NO`; design TBD |
| `Other Users` namespace (`user/alice/INBOX`) actually opens alice's mailbox | ACL-1 + NS-3 |
| Quota debit on writes to `user/alice/*` charges alice (owner-paid) | QUOTA-1 + NS-3 |
| Director routes `user/alice/*` to alice's backend pod in multi-pod deployments | NS-3 |

## Mixed storage drivers across namespaces

The `location:` URL's driver prefix is honoured per-namespace.
When a namespace declares a `location:` whose driver differs from
the globally-configured `cfg.Storage.Mailbox`, yarilo constructs a
separate `MailboxBackend` instance of the requested driver and
routes that namespace's ops through it. Namespaces using the same
non-default driver share their backend instance.

Examples that work out of the box:

```yaml
storage:
  mailbox: maildir              # personal default (existing layout)

namespaces:
  - type: personal
    prefix: ""
    separator: "/"
    list: true
    # personal inherits maildir from storage.mailbox
  - type: shared
    prefix: "Shared/"
    separator: "/"
    list: true
    location: "mdbox:/var/yarilo/shared"   # shared uses mdbox
  - type: shared
    prefix: "Public/"
    separator: "/"
    list: true
    location: "dbox:/var/yarilo/public"    # public uses dbox
```

What it gives you: each namespace gets the storage format best
suited for its access pattern (e.g. mdbox's coalesced storage for
high-volume shared folders, maildir's per-message files for
per-user personal mailboxes that backup tools handle file-by-file).

Constraints:
- `IndexBackend` is uniform (fileindex) across all namespaces;
  yarilo does not switch index implementations per namespace.
- The configured driver in `location:` must be one of
  `maildir`, `dbox`, `mdbox`. Mismatched / unknown driver names
  fail at backend startup so a typo does not silently fall back
  to maildir.
