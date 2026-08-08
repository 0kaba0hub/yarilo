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
modifiers, built by the same `ResolveUserInfo` → `StampLocation` path the
session user goes through — one implementation, so an owner resolves exactly as
a logged-in user does.

**Precedence, decided before the code: the owner's userdb decides the driver and
the root; the namespace template supplies the root only when the userdb gave
none.** This is the same rule the session user already follows (`server.go`:
`if res.MailPath != "" { ... }` -- the userdb path wins when present). The lookup
returns the owner with their real store: `ResolveUserInfo` sets `MailPath` and
`Home` from the owner's `mail_location`, and `StampLocation` sets the driver and
fills the `INDEX=` / `CONTROL=` / `ALT=` / `VOLATILEDIR=` modifiers only where
nothing has set them yet (`resolve.go`).

`resolveOwnerUserInfo` therefore keeps `MailPath`/`Home` from the lookup and
lets the namespace `location:` fill them only when the userdb left them empty --
`fillIfEmpty` on the root, the same gap-fill it does on the modifiers, so the
owner and the session user resolve by one rule, not two. It does **not**
overwrite the root with the template: a deployment with per-user drivers
(mdbox / maildir / sdbox by account, each at its own path -- `~/mdbox`,
`~/Maildir`, `~/sdbox`) has no single template that names all three roots, and
forcing one would point the owner's userdb driver at a template path -- an
`mdbox` driver on a `maildir` tree, the parallel tree this design exists to
prevent. The driver is never overwritten after the lookup for the same reason.

The namespace `location:` must still carry a per-owner variable (config
validation enforces it, since 2.3.84), because in the one case it is used -- an
owner whose userdb gives no `mail_location` -- it becomes the root, and a fixed
location would resolve every such owner to one shared path, the same tree this
design exists to prevent.

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
- **Bound**: the per-session owner-handle cache is capped (v1: 64). When full,
  one cached handle is evicted and closed so a session walking many owners does
  not grow unbounded. v1 evicts an arbitrary entry (insertion order), not strict
  LRU — enough as a memory cap; a hot owner re-resolved after eviction costs one
  userdb lookup. The cache stores the **resolved handle**, not the userdb lookup
  response, and `resolveOwnerUserInfo` copies the looked-up `UserInfo` before
  mutating it, so a future lookup cache that returned a shared pointer could not
  have its entry rewritten under it.

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

### 3.7 One owner, one definition, and where the shortcut lives

Two definitions of "owner" already coexist in the code, and B1 is what forces
them together — not what introduces the second one:

```go
isOwner(h)      (acl_check.go)   -> h.spec.Type == NamespacePersonal   // by namespace type
adminCheckPRc   (acl.go:204)     -> s.userInfo.Username == h.userInfo.Username   // by person
```

The second is already the definition §3.5 needs. So B1 does not add an owner to
a shared namespace; it makes the by-type definition the by-person one, and the
risk is a third appearing rather than a second.

**Decided, before the code:**

**One definition.** `isOwner` becomes "the session user is the owner of this
instance of the namespace", and nothing keeps a private version of it.
`adminCheckPRc` goes and its two call sites use `requireAdminOn` — tracked as
#1107, and to be done **before** B1, so B1 has one admin route to reason about
rather than two. Same order as #1094 before #1096: harden the writing path,
then give it new targets.

**Implicit grant, not a bypass — with the shortcut inside the resolver.** Being
the owner means `Effective()` returns the full right set for that user, not that
the callers return early before resolving anything. The distinction is not
philosophical: with the bypass, `MYRIGHTS` and `GETACL` reach their answers by
different routes and agree by coincidence, which is the exact shape of every
defect this series has caught — the validator against the builder, escaping
against encoding, the FTS tree against the mail tree. Each half right, the pair
not, and nothing saying so.

The cost argument for a bypass does not survive the move: `Effective()` answers
the owner from the definition without reading a file, so the owner is still
free. The shortcut lives one level down, inside the single source every caller
already uses, instead of being repeated in each of them.

It also removes the synthesis in `GetACL`: it stops inventing an
`owner=FullRights` entry when none is stored and shows what the resolver
returned, which is now the same thing.

**The implicit grant beats an explicit negative.** `-user=alice` on the owner
does not remove the owner's rights. Under a bypass the question could not
arise; under an implicit grant it arises on the first test, so it is settled
here. The reason is the same one that makes §7.2 matter: a shared namespace has
no second owner to repair it, so one `SETACL` could make the namespace
unmanageable from inside, with no session able to undo it.

**Owner and root grant are independent.** The owner holds rights because they
are the owner; peers hold what the namespace-root ACL and the per-mailbox ACLs
give them. Stated explicitly because the first deployment of B1 will ask "why a
root grant at all, if there is an owner", and the answer belongs here rather
than being reconstructed afterwards: an owner-templated namespace has an owner,
a fixed shared one does not, and both use the same root.

**#1096 is a precondition, not a happy accident.** Before it, the root ACL was
INBOX's own file — `Path("") == Path("INBOX")`. An owner-templated namespace is
the first to have both an INBOX of its own *and* a namespace-root ACL, so
without the separate `yarilo-acl-root` file B1 would walk straight into #1091 on
its first deployment: a grant meant for the namespace would be read as INBOX's,
or the reverse.

Whether an owner sees the existence-hiding rule of §7.1 is deliberately not
decided here — it is a property of the refusal, so it belongs beside the
refusal.

---

### 3.8 An unexpanded template is not a mailbox

Recorded once so #1138 and #1139 stop deciding it separately: `user/%u` (and
the bare `user/`) name no mailbox. Three consequences, one answer each:

- **LIST/LSUB** never emit the template as a row. The `list` mode defaults to
  `children` for an owner-templated prefix, so the node disappears because the
  namespace is declared children-only. The mechanism deliberately differs from
  the reference's: there the template never appears because the namespace
  prefix itself is truncated at the variable for the process's whole life
  (shared-storage.c) -- its `list = children` still shows a prefix node that
  has children (mailbox-list-iter.c) and hides it only when it has none. Our
  `children` never lists the node. Same wire result for the templated
  namespace, honest to record that it is not the same rule. The literal head
  of the prefix (`user`) IS presented, as `\Noselect \HasChildren` -- a
  client walking down from the advertised prefix finds a container, not a
  hole, and `\HasChildren` is truthful: the caller's own space always exists.
- **NAMESPACE** advertises the prefix truncated at the variable: `user/`, not
  `user/%u/` -- the same truncation the reference applies (#1171). RFC 2342
  defines the prefix as what the client prepends to a mailbox name; a client
  prepending `user/%u/` builds `user/%u/INBOX`, which resolves to nothing --
  the lie #1139 removed from LIST, sitting in the one response whose only job
  is to say what to prepend.
- **A pattern with the owner written out** (`user/alice/*`) materialises that
  owner's namespace and lists it like any other, ACL filtering included; a
  wildcard in the owner segment enumerates nobody, because there is no
  registry of owners to enumerate. An owner that does not resolve and an
  owner whose space is hidden produce the same silence (7.1).

## 4. Config schema

```yaml
namespaces:
  - type: shared                 # or "other" for the Other Users NAMESPACE slot
    prefix: "user/%u/"           # %u in prefix ⇒ owner-templated
    separator: "/"
    list: children               # yes | children | no; unset defaults to
                                 # children for an owner-templated prefix
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
| Owner resolves, peer lacks ACL right but holds `l` | `NO [NOPERM]` naming the missing right | INBOX implicit keep (item 2) |
| Owner resolves, peer holds no `l` right | `NO [NONEXISTENT] No such mailbox` — byte-identical to an absent mailbox | INBOX implicit keep (item 2) |
| Owner == self | personal handle alias | recipient's own store |
| Malformed / traversal owner segment | userdb miss → `NO` | INBOX implicit keep |

### 7.1 Existence disclosure — decided, not deferred

The commands that name a mailbox check that it exists before they check rights,
so a peer could once tell "no such mailbox" from "not allowed" and enumerate
names in a shared namespace it may not see. RFC 4314 §4 permits either answer.

**Decision: the refusal is identical to the absent-mailbox refusal when, and
only when, the peer lacks the lookup right.** With `l` the peer already knows
the mailbox is there, so naming the missing right discloses nothing and is far
more useful to a client and an operator.

It reaches every command that names a mailbox, by two routes:

- `SELECT`, `STATUS`, `DELETE`, `RENAME`, `METADATA`, `APPEND`, `COPY`, `MOVE`
  and the rest go through `requireRight`;
- `GETACL`, `MYRIGHTS`, `LISTRIGHTS`, `SETACL` and `DELETEACL` do not use
  `requireRight` at all — they share `resolveACLHandle`, which applies the same
  rule. They were missed on the first pass, and `GETACL` was worse than an
  oracle while they were: it answered a peer holding no rights with the
  mailbox's full ACL, including the implicit owner entry, which names the owner.

`GETACL` and `LISTRIGHTS` additionally require the `a` right (RFC 4314 §4);
`MYRIGHTS` does not, because it answers only about the caller.

Two things this deliberately is *not*:

- It is **not** a reordering of the thirteen commands that check existence
  before rights. What leaked was the difference between two replies, not the
  order in which they were reached; making the replies equal is a smaller
  change with the same effect, and it keeps the good error messages for owners.
- It does **not** apply to `CREATE`'s parent check. `CREATE` names a mailbox
  that does not exist yet, so "No such mailbox" would be true of the request and
  say nothing about the failure. The disclosure being avoided is about mailboxes
  that *are* there.

Personal namespaces are unaffected: the owner holds every right, so no refusal
of either kind arises. The same resolution is what the reference implementation
reaches (`acl_mailbox_fail_not_found`).

**An owner never meets either refusal**, and once §3.7 makes ownership an
implicit grant inside `Effective()` that stops being a separate statement: the
owner resolves with the lookup right, so the rule that equalises the two
refusals is never reached. That is the fourth surface of the same decision, and
it is stated here rather than in §3.7 because it is a property of the refusal.
It is what makes an owner-templated namespace behave for its owner exactly as a
personal one does, without a second code path saying so.

Tracked as #1068.

**In an owner-templated namespace the rule moves to the resolve layer, and the
write verbs inherit it.** Here the probed segment is a *username*, so the split
this rule governs is no longer "does this mailbox exist" but "does this account
exist" — a directory of the deployment, readable by anyone who can log in. Two
verbs escaped the per-command rule and showed why it cannot stay per-command:
`CREATE` refuses with `NOPERM` by deliberate design (§7.1 above: naming a mailbox
that does not exist yet, "no such mailbox" would tell the user nothing), and
`SUBSCRIBE` checks no rights at all — RFC 9051 §6.3.7 lets it accept an
unvalidated name, so it answered `OK` for a stranger's mailbox. Each was
defensible alone; paired with the `NONEXISTENT` an unknown owner gets, both
became oracles (#1138).

So the owner-templated resolver decides visibility once, before any verb runs: a
caller who holds **no right at all** on the addressed name is told exactly what an
unknown owner is told, from one error value used for both. A caller holding any
right is not hidden from — they already know the space is there, so the precise
refusal discloses nothing, and hiding from them would break the namespace rather
than protect it. The owner is never hidden from their own space. Deciding it at
the resolver is what makes verbs added later inherit the answer instead of each
needing its own patch — and it is why an unreadable ACL hides too: that failure
is only reachable for an owner that resolved, so reporting it would answer the
question the hiding refuses (it is logged instead).

### 7.1a Subscriptions follow the subscriber

A subscription is the subscriber's state, not the mailbox's. An owner-templated
namespace kept its own subscription file **in the owner's store**, so a peer's
SUBSCRIBE wrote a row into a stranger's file -- a row the peer could not remove
and never saw, while the owner saw one they never made.

Subscriptions for such a namespace now live in the namespace that keeps them --
the subscriber's own, normally the personal one -- under the client-visible name
(`user/alice/Sent`), which is what keeps them distinct from the subscriber's own
`Sent`. The storing namespace is the one that keeps subscriptions and whose
visible prefix is a prefix of the name; the key is the name minus that prefix.
Where no such namespace exists (a personal namespace with a prefix of its own,
addressed outside it) the command is refused rather than writing a key nothing
would match.

**Divergence, and it is narrower than it first looks.** 2.4 defaults
`subscriptions` to *yes* for every namespace type, checks only that at least one
namespace has it, and never inspects the type -- so a shared namespace with
`subscriptions=yes` really does write into the owner's store there too, guarded
only by filesystem permissions and `mail_control_path`. We have neither barrier:
every process runs under one uid on a shared RWX volume. So:

- fixed shared/public keep the reference default (**yes**) -- there a shared
  subscription file is a real feature, a site-wide list, and an operator turns it
  off deliberately;
- owner-templated is the one place we refuse `yes` at startup, because its
  storage is resolved per owner at runtime: "the namespace's own subscription
  file" names no owner at all. That is a configuration without a meaning rather
  than a dangerous one, which is why it is a startup error and not a policy.

**Migration.** Authorship was never recorded in those files, so a peer's
subscription cannot be given back to the peer. Every row in an owner's file
names a mailbox in the owner's own space, so folding them into the owner's own
file restores the owner's subscriptions exactly, and the only imprecision is
that the owner inherits rows a peer created -- all pointing at mailboxes the
owner already sees. Deleting the files instead would have removed the owner's
own subscriptions silently, with folders vanishing from their client and no
trace: the one outcome with no signal. `yarctl backend subscriptions migrate
<user> --namespace NS` folds them, dry run unless `--apply`.

### 7.2 The bootstrap grant — why `k` alone leaves a namespace nobody can clean up

A shared namespace starts empty and grantable only at its root: nobody can
create its first mailbox without the create right, and there is nowhere else to
put that right. The grant goes on the root, addressed with `--root` on the CLI
and `"root": true` on the wire.

An incomplete grant produces a namespace that fills up and cannot be emptied:

```
u2: CREATE   "Public/Reg69"    OK
u2: SELECT   "Public/Reg69"    OK [READ-WRITE]
u2: MYRIGHTS "Public/Reg69"    lrsk
u2: DELETE   "Public/Reg69"    NO [NOPERM] Permission denied: missing right 'x'
```

That `k` (create) and `x` (delete mailbox) are separate rights is RFC 4314 and
needs no documenting here. Two things about how they behave in this model are
not in the RFC, and both are why the recipe below matters.

**A shared namespace has no owner.** `isOwner` is true only for a personal
namespace, where "I created it, so I can delete it" holds through the owner
shortcut rather than through any ACL. That shortcut exists for nobody here. So
the consequence of a grant without `x` is not "somebody else needs the right" —
it is that **no user of the namespace holds it at all**. An operator carrying
intuition over from the personal namespace gets a mailbox nobody can remove.

**Children inherit the root grant.** A mailbox created in the namespace inherits
exactly what the root granted — `MYRIGHTS` above returns the root's `lrsk`
verbatim. An incomplete bootstrap grant is therefore not a local mistake: it is
replicated onto everything created in that namespace afterwards.

It is not irreversible — `yarctl acl set ... --root` can add rights later — but
it is not fixable from inside a session, which is why the recipe belongs here
rather than in someone's head.

**The recipe.** For a manageable shared namespace the root grant needs at
minimum **`lkx`** — see it, create in it, delete from it. In practice
**`lrswipkxte`**. Add **`a`** for whoever is allowed to delegate further: with
`a` the grantee can repair the rest themselves, without it every later
adjustment needs an administrator.

The letter that lets a peer *add* mail over IMAP is **`i`** (insert), the same
in a shared namespace as in a personal one — not `p`. `p` (post) is the
delivery right, checked only on the LMTP path; an IMAP `APPEND`/`COPY`/`MOVE`
never consults it (#1119). The practical `lrswipkxte` carries both letters, so
it works either way; a minimal grant for a peer who should `APPEND` must
include `i`, and `p` alone will not do it.

```
yarctl backend acl set --root <owner> <grantee> lrswipkxte --namespace public
```

**The deploy check depends on this.** `checkACLDisclosure` (see `docs/SMOKE.md`)
creates `<prefix>SmokeAclProbe` and removes it afterwards, which needs `x`.
Since 2.3.70 a cleanup it cannot complete fails the check rather than printing a
note, so a smoke user granted without `x` reports a failure on every run instead
of quietly leaving another probe mailbox behind.

Tracked as #1104.

### 7.3 Inheritance is materialised at creation, not resolved at every check

When a mailbox is created it is given an ACL file of its own carrying what it
inherits — the first ancestor with an ACL, else the namespace-root default.
After that the file is authoritative: nothing is layered underneath it.

**The file is the answer to "who has rights here".** That is the reason, ahead
of parity. Audits, backups, restores and incident work all read files; if
inheritance were resolved live, the same file would mean different things before
and after an administrator edited the root, and no snapshot of the disk would be
self-contained.

It also removes #1111 at the source. `u2`, holding the create right at the
namespace root, creates `Public/Matrix`; the root's entry for `u2` is written
into the new mailbox's ACL there and then, so the first `SETACL` `u2` issues on
it does not replace the grant they are acting under. There is nothing to shadow.

A mailbox with **no** file of its own is not given one, by creation or by the
repair below: it still resolves live through its ancestors to the root default,
exactly as the reference's `acl_backend_vfile_object_init_parent` does on
lookup. So the section title holds for the mailboxes that have a file, which is
what makes `materialise` a repair rather than a migration everybody has to run.

**Global ACLs are not copied.** They keep merging live at resolve time, which is
what a global ACL is for, and the reference excludes them from the copy for the
same reason (`if (!update.rights.global)`). In yarilo they cannot be copied even
by accident: the global ACL is operator configuration and never a stored entry.
A creator whose rights come from it holds them without appearing in the file.

The alternative — merging the namespace root under every mailbox at resolve
time — was implemented first and reverted. It fixes this case by making a
per-mailbox ACL *additive*: every identifier the root names regains its rights on
every mailbox, including ones whose file was written to leave them out.
Restriction by omission stops working, silently, and that is the wrong direction
for access control. Merging is the global ACL's semantics, one layer up, and the
root is a different thing wearing a similar shape.

**Two consequences worth stating rather than discovering:**

- Every mailbox created in a namespace that has a root ACL now has an ACL file
  of its own. That is the point — it is what makes the state readable — but it
  is more files than before.
- A later change to the root does **not** reach mailboxes that already exist.
  Changing policy across a namespace is an administrative operation over its
  mailboxes, which makes #1109 a precondition for operating this rather than a
  convenience.

**Repairing what predates it.** `yarctl backend acl materialise <user> <folder>…`
adds, per mailbox, the inherited identifiers the mailbox does not already name.
It is a dry run unless `--apply`, it never rewrites an entry that is already
there, and running it twice adds nothing. It is deliberately not automatic: a
mailbox orphaned by the old rule and a mailbox whose ACL leaves out an
identifier on purpose are the same file on disk, so a resolver doing this on
read would widen access exactly where it was narrowed deliberately.

The report names the rights, not just the identifiers, because that judgement is
the operator's and two bare names print a repair and a widening identically:

```
$ yarctl backend acl materialise public Matrix Sales --namespace public
added = {"Matrix": [{"identifier":"user=u2","rights":"lrskxa"}],
         "Sales":  [{"identifier":"anyone","rights":"lr"},
                    {"identifier":"user=u2","rights":"lrskxa"}]}
```

The first line is the repair; the second is `anyone` gaining read access to a
mailbox whose ACL deliberately named one person, which is visible as a mistake
at a glance only because the rights are there. `skipped` carries the rights the
mailbox's own entry keeps giving, for the same reason.

Name the mailboxes. The API takes an explicit list and offers no way to sweep a
namespace, so that widening a whole namespace cannot be one keystroke; a recipe
written with a wildcard would quietly reintroduce what the interface withholds.

Tracked as #1111.

### 7.4 Duplicate ACL entries: last statement wins, not union

An ACL file holds at most one entry per identifier per sign. Duplicates are
collapsed when the file is read and when it is written, and where two lines name
one identifier with one sign the later line is the one that counts.

This is a deliberate divergence from the reference, which merges duplicate
records (`acl_right_names_merge`, `acl-api.c:262-266`). The histories differ, and
so the default does: our duplicate corpus is machine-generated — by a CLI that
appended instead of replacing — where the later line is literally the write the
operator asked for; a Dovecot file acquires a duplicate only from hand-editing,
where merging is the kinder reading. Union was also the behaviour that made an
ACL impossible to reduce, so merging would have fixed the accumulation and kept
the defect.

The consequence to keep in mind once the write path no longer appends: the only
remaining source of a duplicate is hand-editing, and there last-wins silently
drops rights an operator listed on purpose — `user=alice lr` then
`user=alice s` resolves to `s` here and to `lrs` in Dovecot. That is an accepted
trade today; it is written down so it is not later rediscovered as a bug.

### 7.5 Evaluation order and the storage-name byte layout

Two more places yarilo and the reference reach the same result by different
means, recorded so a migration does not read them as faults:

- **Rights evaluation is equivalent, the record shape is not.** The reference
  folds an identifier's positive and negative rights into one record and decides
  the tier boundary once; yarilo keeps them as separate lines and decides the
  boundary once per sign. Same answer, including the owner short-circuit, the
  global-over-local ladder and negative subtraction — but a yarilo ACL file lists
  a negative on its own line where a Dovecot one carries it in the identifier's
  record.

- **NFC is applied once, at the name-entry boundary.** The reference normalises
  in `mailbox_alloc`, the single point every protocol funnels through; yarilo
  has no such shared point across protocols, so normalisation lives in
  `mailbox.NormalizeName`, called inside each namespace resolver (IMAP dispatch,
  LMTP delivery, the admin API, the migrator). Path derivation -- the drivers'
  disk-name step and `FolderSubpathEscaped` -- takes the name as given and never
  normalises, so the order of NFC against escaping, which used to be held by
  convention across two owners, no longer exists to get wrong (#1078, #1092,
  #1113).

- **Escape-before-encode is the reverse of the reference, and it is a
  byte-compatibility boundary.** yarilo escapes the storage name and then
  encodes to modified UTF-7; the reference encodes first and escapes second
  (`mailbox-list.c:316-343`), which it can because its escaper is mUTF-7-aware.
  Both are self-consistent, but a folder name containing **both** a non-ASCII
  character and the escape character is stored as different bytes on disk under
  the two. A store migrated from Dovecot is not byte-identical for such a name;
  it must be re-derived through yarilo, not copied. This belongs in the migration
  runbook, not in a code change.

Tracked as #1117.

---


### 7.6 Owner resolution follows the userdb, not the reference's no-lookup path

Two deliberate divergences from 2.4, both because this deployment carries
per-user storage the reference's shared-namespace model does not express.

- **Full userdb lookup, not `no_userdb_lookup`.** 2.4 resolves a shared
  namespace's owner without a userdb round-trip (`no_userdb_lookup = TRUE`),
  deriving the owner's home from the template alone. It can, because its shared
  spaces are homogeneous by assumption — one driver, one layout for every owner.
  Ours are not: the userdb assigns a driver per account (mdbox / maildir / sdbox
  by range), and only a full userdb lookup surfaces it. Following the reference
  here would import a limitation the deployment already violates, so the owner is
  looked up in full and the result cached to pay the round-trip once (§3.4).

- **The driver and the root come from the owner's userdb, not the namespace
  `location:`.** The same fact drives §3.3's precedence: the deployment's
  per-user drivers live in the owner's `mail_location`, and so does the owner's
  real root. The namespace template supplies the root only for an owner whose
  userdb gives no `mail_location`; otherwise the userdb wins both. A template
  that overwrote the root would point a per-user driver at a path it does not
  match — the parallel-tree bug.

- **The owner grant is strong: no ACL entry, negative, or global reduces the
  owner.** 2.4 places the owner tier *below* `ACL_ID_USER`, so an explicit
  `user=<owner>` entry there caps the owner and a global ACL still applies to
  them (`acl-api.c:323-338`). yarilo resolves the owner above every tier: the
  owner is decided at the boundary (`Effective` / `EffectiveWithGlobal`
  short-circuit to `FullRights`), so a `-user=<owner>` negative, a reduced
  `owner rw` entry, and a global negative all leave the owner at full. The
  reason is the invariant this whole section defends — an owner-templated
  namespace also has no second owner to repair one `SETACL` — but it bites
  hardest on the *personal* namespace, which shares the same ACL file: the owner
  is the same principal for `user/alice/Sent` and alice's own `Sent`, so the
  weak form would have let one entry lock alice out of her own mail on the
  hottest path. Two consequences follow and are deliberate:
  - **Capping the owner through the ACL is no longer possible** — not by a
    negative, an explicit entry, or a global. Because such a write would answer
    OK and change nothing (the shape #1114 removed), an owner-naming entry — the
    owner's `user=` identity of either sign, or the now-inert `owner` keyword — is
    refused on write, and GETACL shows the single resolved owner row rather than
    the inert stored entry beside it. Freezing a mailbox (a suspended account, a
    legal hold) is a separate mechanism, not an ACL edit; calling it a bug would
    be reading this divergence as an oversight.

    The two write surfaces are deliberately asymmetric. IMAP refuses every
    owner-naming write; the admin API refuses only a *grant* (add, or replace
    with rights) and leaves *removal* (remove, replace-with-empty, delete the
    file) working. The reason is this very filter: GETACL hides owner residue and
    IMAP will not delete it, so if the admin API also refused removal, a file that
    already carried `-user=alice …` or `owner lr` would be invisible, inert, and
    indelible. The admin API is the one path that can clear it.
  - **The global ACL loses to the owner.** #1118 made global entries *replace*
    local ones, so without the boundary short-circuit an operator-set global
    negative would strip the owner — stated because the next reader, seeing
    global override everywhere else, would otherwise take this for a gap.

These are the shape of §7.5's escape-order note: the reference is self-consistent,
and yarilo diverges on purpose where the deployment's shape demands it. Written
down so a later reader does not "restore parity" by dropping the lookup and
reintroducing the per-user-driver bug through config, or by lowering the owner
back under `user=` and reintroducing the lock-out.

The IMAP resolver and the admin ACL API both build the owner identity through one
producer (`mailbox.StampOwnerLocation`): each owns its userdb lookup — the IMAP
on-demand cache, the admin `AuthClient` — but the precedence lives in one place,
so neither can drift into the template-root bug the other avoids (#1142). The
admin path derives the owner from the addressed mailbox name, exactly as IMAP
does, and `req.User` (when given) must equal it; the acting operator is a separate
`actor` field, so a cross-owner edit holds its lock under the operator, not the
store owner.

- **An unresolved owner is a `NO [NONEXISTENT]`, not an unusable namespace.**
  2.4 marks a shared namespace whose owner does not resolve with
  `NAMESPACE_FLAG_UNUSABLE`, because there the namespace is already built by the
  time the owner is checked. Here the userdb lookup precedes the handle, so a
  miss is answered before anything is constructed -- cheaper and simpler, and a
  userdb miss already means "no such user". Deliberate.

### 7.7 Every ACL surface reads back what the evaluator resolves

Three defects with three different causes shared one consequence: stored ACL
state could not be read back by the same path that wrote it. #1109 -- the admin
API read the wrong store, so shared-namespace folders were invisible to it.
#1144 -- the admin API and IMAP resolved the same mailbox name to two different
stores, so each surface read back its own write and the two still disagreed
with each other: agreement with the evaluator is necessary, and not sufficient
unless the surfaces also resolve the same state. #1147 -- IMAP
`SETACL`/`DELETEACL` updated the per-mailbox file but not the
`yarilo-acl-list` index, so `yarctl acl list` named grants that were changed
or removed.

(An earlier revision cited #1145 -- `GETACL` hiding negative entries -- as the
third case; that report was retracted as invalid: the reproduction used
`SETACL <mailbox> <id> -w`, which is RFC 4314 rights *removal*, not a negative
identifier, so nothing negative was ever stored. GETACL lists negative entries
correctly.)

The absent invariant, stated once:

> **Every surface that shows an ACL must show what the evaluator would resolve
> from the same inputs.** GETACL, `yarctl acl get`, `yarctl acl list`, and
> enforcement all read the one stored state; none may show a view the evaluator
> would not.

This converts each such defect into a single per-surface test -- write through a
surface, read back through it, compare against the evaluator -- rather than three
separate investigations that each had to start from a symptom. The #1147 fix
follows it structurally: `Set` is `Update` with a constant function, so the file
and the index are written in one place with no second path to forget the index.

### 7.8 A malformed ACL line fails the whole file, deliberately

The reference logs a malformed `vfile` line and keeps what it read; we abort
the parse with the line number and serve nothing (`ParseACL`). That is a
decision, not an accident: a partially-read ACL silently drops entries, and a
dropped *negative* entry widens access — the one direction a parser error must
never widen. Fail-closed costs availability of one folder's ACL; fail-open
costs the subtraction someone wrote on purpose.

Recorded here so it is not "fixed" later by matching the reference without
this trade-off in view. The adjacent changes shrink what the abort can hit:
identifiers are validated before every write (length, UTF-8, control
characters — the reference's own bound, which the reference itself enforces
only on its IMAP write path), and identifiers containing spaces use the
reference's own quoted encoding (`"user=John Smith" lrw`, backslash-escaped,
the negative sign inside the quotes) — so a file migrated in either direction
round-trips byte-compatibly, and an operator can no longer legally write a
line the parser then refuses to read back.

### 7.9 Smaller recorded divergences, validated against the sources

- **`subscriptions` startup refusal.** The reference defaults `subscriptions`
  to yes for every namespace type and checks only a lower bound (at least one
  subscriptions=yes namespace) at startup. We refuse explicit
  `subscriptions: true` on an owner-templated namespace. Consequence worth
  stating: a working reference config with that setting does not start here --
  by design, since the setting configures a file that names no owner.
- **Auto-subscribe on first SELECT.** The reference has no such behaviour at
  all (only `autocreate`'s subscribe-on-create). Ours subscribes on the first
  SELECT so LSUB shows the folder without a client round-trip. Long-standing,
  now recorded.
- **The owner in a shared namespace.** The reference unconditionally clears
  ownership for any non-private namespace (acl-backend.c) -- nobody owns
  `shared/alice/*` there, alice included; her implicit rights come back only
  through the ACL default. Ours resolves the owner of an owner-templated
  instance and gives them the owner tier. Interaction with `acl_ignore`
  (IgnoreACL) is the open edge to watch: those are exactly the lines the
  reference's clearing disables.
- **`--root` / `"root": true` on the admin request.** Not a semantic
  divergence but a workaround for two format limits: the reference's admin
  CLI takes the mailbox as a positional argument, so the namespace root
  cannot be addressed there at all, and in JSON an absent `folder` and
  `folder: ""` decode identically. On output both implementations use the one
  field, and the empty name IS the root.
- **CREATE under a namespace the caller may not see.** The reference answers
  Permission denied -- with its own comment conceding the existence
  disclosure -- and an internal error for a nonexistent owner, so CREATE is an
  account oracle there. #1158 equalised both answers here. Stricter than the
  reference, deliberately, not parity.

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
