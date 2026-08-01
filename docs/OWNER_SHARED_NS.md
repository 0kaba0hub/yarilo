# Owner-templated shared namespaces + dynamic per-owner resolution

Design for **#499 item 3**. Scope: make a shared / other-users namespace
resolve to **per-owner** storage by expanding the location template against
the *owner's* `UserInfo` (looked up from the userdb on demand), instead of the
single fixed path every session sees today.

This is the yarilo equivalent of the reference's `index/shared/shared-storage.c`.

Status: **design** — no code yet. Items 1 (delivery-through-namespaces, #503)
and 2 (POST-right, #504) are done and live-verified.

---

## 1. Problem

`internal/imap/dispatch.go` opens shared / other namespaces at login with:

```go
loc, ok, err := mailbox.ParseLocation(spec.Location, nil) // nil UserInfo
```

`ParseLocation` only expands `%u/%n/%d/%h` when it is handed a non-nil
`UserInfo` ([pkg/mailbox/path.go](../pkg/mailbox/path.go) `ParseLocation`
→ `ExpandVars`). With `nil`:

- `%h` → `""`, `%u/%n/%d` → empty-user expansion,
- the namespace resolves to **one fixed path for every session**.

So `location: "maildir:/var/yarilo/shared"` works (no variables), but
`prefix: "user/%u/"` + `location: "maildir:%h"` — the per-owner shape — cannot
resolve: there is no owner to expand against, and the owner isn't even known
until a specific mailbox name (`user/alice/Sent`) is referenced.

The templating engine exists and is unit-tested
([pkg/mailbox/path_test.go](../pkg/mailbox/path_test.go)); it is simply not
wired to an owner identity.

---

## 2. Reference implementation

```
namespace {
  type = shared
  separator = /
  prefix = user/%%u/
  location = maildir:%%h/Maildir:INDEX=~/shared/%%u
  subscriptions = no
  list = children
}
```

- **`%%u` / `%%n` / `%%d` / `%%h` = the mailbox OWNER**; single `%u` = the
  logged-in user. The doubling is only the reference's config-parser escaping; the
  runtime distinction is "owner var" vs "session var".
- On first access to `user/alice/*`, `shared-storage.c:159`
  (`shared_storage_get_namespace`) parses the owner out of the name, does a
  **userdb lookup** of `alice` to get her home / mail location, expands the
  template against *alice's* `mail_user`, and creates the storage + namespace
  **on demand**, caching it on the session.
- Owner identity for ACL: `acl-backend.c:78-80` — a mailbox is *owned* only
  when `username == owner && type == PRIVATE`. Shared/public roots have no
  owner, so access is purely ACL-driven. For `user/alice/*` the owner is
  **alice**: alice's own session sees `isOwner == true` (full implicit
  rights); bob's session sees `isOwner == false` and needs an explicit grant
  from alice.
- Cross-user delivery routes the same way: `mailbox_alloc_for_user` →
  `mail_namespace_find(name)` picks the shared namespace, resolves the owner,
  and `acl_save_begin` checks `p` (POST) — exactly the path items 1+2 already
  mirror for the fixed public root.

---

## 3. yarilo design

### 3.1 Owner variable convention

yarilo config is plain YAML/koanf — there is no config-parser doubling, so the
`%%` escape is unnecessary and would be confusing. Instead:

> **A namespace is *owner-templated* when its `prefix` contains an owner
> variable (`%u`, `%n`, or `%d`). Inside such a namespace, the `%u/%n/%d/%h`
> variables in `location` refer to the OWNER, not the session user.**

Rationale: the owner variable only ever makes sense in the prefix of a
shared / other namespace (`user/%u/`), and once the prefix declares it, every
location variable in that namespace is unambiguously about the owner. This
keeps one variable vocabulary (`ExpandVars`) and needs no new escape syntax.
Fixed namespaces (no variable in the prefix, e.g. `Public/`) keep resolving
exactly as today.

Non-owner-templated shared namespaces (`Shared/`, `Public/`) are unchanged.

### 3.2 Owner extraction

For a mailbox name under an owner-templated namespace, the owner is the single
name segment that fills the variable slot of the prefix, delimited by the
namespace separator.

```
spec.prefix    = "user/%u/"          separator = "/"
name           = "user/alice/Sent"
                       └────┘ owner = "alice"
rel            = "Sent"               (folder within the owner's store)
name           = "user/alice"    →   owner = "alice", rel = "INBOX" (bare)
```

`%n` / `%d` variants: `prefix = "user/%n@%d/"` extracts `alice@example.com`
across the two slots. v1 implements the common `%u` (full username) form;
`%n`/`%d` split-slot prefixes are a documented follow-up if needed.

**Validation (security):** the extracted owner is a userdb key, never a path
component. It is passed to the userdb lookup as-is; the resolved storage path
comes only from the userdb + template, never by concatenating the raw owner
segment into a filesystem path. This blocks `user/../../etc/` traversal — a
non-existent or malformed owner simply fails the userdb lookup → `NO`.

### 3.3 Owner userdb lookup

Both entry points already have (or can be handed) a userdb-master client:

- **LMTP** — `lmtp.Options.UserdbLookup(ctx, username) (*mailbox.UserInfo, error)`
  already exists ([internal/lmtp/server.go](../internal/lmtp/server.go)), backed
  by the `yarilo-auth` master (`AuthService.MasterAddr`).
- **IMAP** — add `imap.Options.UserdbLookup` with the same signature, wired in
  `backend.go` from the same auth-master client the LMTP path uses. The IMAP
  session only has a passdb (`opts.Auth`) today; owner resolution needs the
  userdb-master lookup for an arbitrary (non-authenticating) user.

The lookup returns the owner's `Home`, `MailPath`, `Driver`, and mail-location
modifiers. `ParseLocation(spec.Location, ownerUI)` then expands the template
against the owner.

### 3.4 On-demand handle construction + caching

Owner handles cannot be pre-opened at login (the set of owners is unbounded and
unknown). Resolution is lazy, per referenced owner, cached for the session:

```
dispatch(name):
  spec := matchOwnerTemplated(name)          # prefix contains %u/%n/%d
  if spec != nil:
     owner, rel := extractOwner(spec, name)
     h := s.ownerHandles[spec.prefix + owner] # session cache
     if h == nil:
        ownerUI, err := s.opts.UserdbLookup(ctx, owner)   # NO on miss
        loc := ParseLocation(spec.Location, ownerUI)
        h = openHandle(spec, ownerUI-derived, owner=owner) # owner set!
        s.ownerHandles[key] = h
     return h, rel
```

- **Cache key**: `spec.Prefix + "\x00" + owner`. One `nsHandle` (mailbox +
  index + subscriptions + ACL store) per (namespace, owner) for the session's
  lifetime; closed in `closeHandles()` alongside the static handles.
- **fileindex**: the cache-key fix from #503 (index keyed by resolved storage
  root, not username alone) already makes distinct owners get distinct index
  state — a prerequisite this design depends on.
- **Bound**: cap the per-session owner-handle cache (e.g. 64) with LRU
  eviction + close, so a session that walks thousands of owners does not grow
  unbounded. `log()` an eviction at debug.

### 3.5 ACL owner tier

The `nsHandle` for an owner-templated namespace carries `owner = <resolved
owner>` (unlike fixed shared/public, whose owner is `""`). Then:

- `isOwner(h) := (h.spec owner-templated) && (s.userInfo.Username == h.owner)`
  — replaces the current `spec.Type == NamespacePersonal` check for these
  handles.
- The owner's own session (`user/self/...`) → `isOwner == true` → full
  implicit rights, no ACL file needed (matches the reference PRIVATE ownership).
- A peer (`bob` opening `user/alice/...`) → `isOwner == false` → the existing
  `EffectiveFor(...)` ACL resolution gates every operation: `r` to SELECT,
  `p`/`i` to APPEND, `l` for LIST visibility, etc. This reuses **all** of the
  #490 ACL machinery unchanged.
- Delivery (LMTP): the recipient is the session identity; delivering to
  `user/alice/Foo` as a Sieve action in bob's… — note delivery runs as the
  *recipient*, so cross-owner delivery means the recipient posting into another
  owner's folder, gated by `p`, identical to item 2's public path.

### 3.6 Self-access shortcut

`user/<self>/X` (owner == session user) should resolve to the session's own
personal storage (same paths the personal namespace uses), not a second handle
opened via userdb. Detect `owner == s.userInfo.Username` and alias to the
personal handle with `rel = X`. Avoids a redundant userdb round-trip and keeps
one index/lock domain for the user's own mail.

---

## 4. Config schema

```yaml
namespaces:
  - type: shared                 # or "other" for the Other Users NAMESPACE slot
    prefix: "user/%u/"           # %u in prefix ⇒ owner-templated
    separator: "/"
    list: true                   # NS-4 will drive per-owner LIST enumeration
    subscriptions: false
    location: "maildir:%h"       # %h/%u/%n/%d ⇒ the OWNER's storage
    acl_ignore: false
```

- `helm/values.yaml` + `helm/templates/configmap.yaml` already render the
  `namespaces:` block including `location:` — **no new keys**. Document the
  owner-var semantics in [NAMESPACE.md](NAMESPACE.md).
- Backward compatible: a prefix without `%u/%n/%d` is a fixed namespace,
  resolved exactly as today.

---

## 5. Delivery path (LMTP)

`deliveryTarget()` ([internal/lmtp/server.go](../internal/lmtp/server.go)) gains
owner-templated resolution symmetric to the IMAP dispatcher:

1. `matchNamespace(folder)` already picks the longest-prefix namespace. Extend
   it to recognise an owner variable in the prefix and extract the owner.
2. `UserdbLookup(ctx, owner)` → owner `UserInfo`; `ParseLocation(loc, ownerUI)`.
3. Build the target box/idx against the owner's store; `rel` is the folder
   within it.
4. POST-right check (item 2, already implemented) with `isOwner` computed from
   the resolved owner: a recipient posting into their *own* templated folder
   skips the check; posting into another owner's folder needs `p`.
5. On userdb miss / owner on a different farm tag (data on a PV this pod does
   not mount) → fall back to the recipient's INBOX (implicit keep), `Warn`.
   No mail loss.

---

## 6. Deployment topology impact (NS-3 boundary)

### Routing model (farms)

The director pins **mailboxes to farms**. A *farm* is a backend or a group of
backends that share **one storage PV**, identified by a **unique tag**. Every
mailbox carries a farm tag, and the tag determines **which PV physically holds
its data**. All access to a mailbox routes only to its farm.

### The one precondition: same farm tag = same PV

For the pod running a session to reach another mailbox, that mailbox's data must
be **physically reachable** — on the **same PV** the pod mounts. That holds if
and only if both mailboxes carry the **same farm tag**. This is the single
discriminator for owner-templated resolution: not "which pod", but **"is the
owner's mailbox on the same farm (same PV) as this session's mailbox?"** — the
tags being unique farm identifiers (e.g. `farm-a`, `farm-b`), not user names.

### Same farm tag — resolution is local

When the owner and the accessing mailbox carry the **same farm tag**, the
owner's data is on a PV the session's pod already mounts. Resolution opens the
owner's storage directly — userdb lookup of the owner, `location` template
expansion, an ordinary `nsHandle`. This covers **standalone** and any
**single-farm backend**. No topology change → **no SVG change**.

### Different farm tag — needs NS-3

When the farm tags differ, the owner's data is on a **different PV** the
session's pod does **not** mount — it is physically unreachable locally. The
director cannot move the session there (its own mailbox is pinned to its own
farm), so just the **owner-access leg** must route to a pod in the farm that
owns that PV. This cross-farm routing is **NS-3** (director-driven), which is
**item 4's** phase.

Boundary for item 3:

- Resolve + open owner storage **only when the farm tags match** (data on the
  same PV the session already mounts). Always true in standalone and single-farm
  backend.
- When the owner is on a **different farm tag**, return the existing
  `NO "... requires NS-3 (cross-pod routing)"` for IMAP and the INBOX
  implicit-keep fallback for LMTP, until NS-3 lands.

**Schema/doc updates:**

- `docs/DEPLOYMENT.md` + `ARCHITECTURE.md` NS table — clarify that
  owner-templated resolution is same-farm in item 3; cross-farm is NS-3
  (item 4). *(Text-only; done alongside this doc.)*
- `docs/yarilo_backend.svg` — **NS-3 will add** a cross-farm "owner-storage
  routing" edge (accessing pod → a pod in the owner's farm, via director).
  Deferred to the NS-3/item-4 PR so the diagram changes land with the code that
  implements the edge, rather than depicting an unimplemented path now.

---

## 7. Failure modes

| Situation | IMAP | LMTP |
|:---|:---|:---|
| Owner not in userdb | `NO` (mailbox does not exist) | INBOX implicit keep + `Warn` |
| Owner mailbox on a different farm tag (different PV) | `NO "requires NS-3"` | INBOX implicit keep + `Warn` |
| Owner resolves, peer lacks ACL right | `NO NOPERM` (existing) | INBOX implicit keep (item 2) |
| Owner == self | personal handle alias | recipient's own store |
| Malformed / traversal owner segment | userdb miss → `NO` | INBOX implicit keep |

---

## 8. Testing plan

- **Unit (`pkg/mailbox`)**: owner extraction from names for `%u` prefixes
  (bare prefix → INBOX, nested folder, trailing separator, no-match).
- **Unit (`internal/imap`)**: owner-templated dispatch resolves a peer's store
  via a stub `UserdbLookup`; owner-tier `isOwner` true for self, false for
  peer; peer `SELECT` gated by seeded ACL (`r`); self full access without ACL;
  handle caching (one lookup per owner); LRU eviction closes the evicted handle.
- **Unit (`internal/lmtp`)**: `deliveryTarget` owner-templated routing +
  POST-right for a peer; self-post skips the check; userdb miss → INBOX
  fallback.
- **E2E (sandbox)**: two users; alice grants bob `lrp` on `user/alice/Project`;
  bob `SELECT user/alice/Project` reads; bob Sieve `fileinto "user/alice/Project"`
  delivers (POST `p`), and without the grant falls back to bob's INBOX.

---

## 9. Implementation checklist (phased, one PR each)

1. **Owner-extraction primitive** in `pkg/mailbox` — `OwnerTemplated(prefix)
   bool`, `ExtractOwner(prefix, name, sep) (owner, rel string, ok bool)` +
   table tests. Pure, no I/O.
2. **IMAP userdb wiring** — `imap.Options.UserdbLookup`, populated in
   `backend.go` from the auth-master client.
3. **IMAP on-demand owner handles** — dispatcher resolution + session cache +
   owner-tier `isOwner` + self-alias + local-only guard (NS-3 boundary).
4. **LMTP owner-templated delivery** — extend `deliveryTarget` + POST-right
   with resolved owner.
5. **Docs + sandbox** — NAMESPACE.md owner-var semantics, a sandbox
   `user/%u/` namespace, e2e verification.

Each step is independently shippable and testable; steps 3 and 4 depend on 1
and 2.

---

## 10. Out of scope (tracked elsewhere)

- **Cross-farm owner routing (NS-3)** — item 4; the director leg that routes the
  owner-access leg to a pod in the owner's farm when the owner's mailbox is on a
  different farm tag (different PV). This design fails closed (`NO` / implicit
  keep) until then.
- **Per-owner LIST enumeration** (`LIST "" "user/%"` showing every owner you
  can see) — needs the dict-backed share discovery, item 5. This design
  resolves an *explicitly named* owner; it does not enumerate owners.
- **`%n`/`%d` split-slot prefixes** — v1 does `%u` (full username); split forms
  are a follow-up.
- **Owner-paid quota** on cross-owner writes — QUOTA-1.
