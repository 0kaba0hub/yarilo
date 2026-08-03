# Full-text search (FTS) — design

Status: **design / plan, revision 4** (issue #250, Phase FTS-1). Revisions 1–3
were reviewed before implementation; slices land per §14.

Full-text indexing of message bodies and headers so that IMAP `SEARCH BODY`,
`SEARCH TEXT` and `SEARCH HEADER` are answered from an index instead of a
brute-force scan.

**Bar:** not worse than the reference 2.4-generation FTS stack (core framework
plus its Xapian engine) on any axis, and measurably better on the axes listed
in §12. The design is grounded in a full source-level analysis of that stack,
including a limitations audit of its Xapian engine; the findings are recorded
here so the implementation can be checked against them.

---

## 1. Goals

- Answer `SEARCH BODY` / `TEXT` / `HEADER <field>` from a full-text index;
  fall back to the existing sequential scan when the index is unavailable or a
  lookup fails.
- **Pluggable engines** behind one interface. The **first implemented engine is
  `flatcurve`** (Xapian). A pure-Go engine (Bleve v2 / scorch) is a
  **separate follow-up stream** (§14) that plugs into the same interface and
  carries the position-dependent improvements (phrase search) plus its own
  `fts_bleve_*` keys.
- **A dedicated `yarilo-fts` service** that owns the indexes — sole writer *and*
  the lookup endpoint — with asynchronous indexing, on-demand catch-up at
  search time, autoindex on delivery, and manual rescan/optimize. Embedded mode
  for tests and single-process CLI runs, remote mode in every k8s deployment —
  the exact `yarilo-locks` precedent. A welcome consequence: **cgo/libxapian is
  confined to the single `yarilo-fts` binary** — every session binary stays
  pure Go; only the fts Deployment uses the cgo image.
- Config key names follow the established mail-server conventions
  (`fts_autoindex`, `fts_flatcurve_*`, …) so operator knowledge transfers.
- RFC-correct substring behaviour where the reference engine is not (see §3);
  RFC-correct phrase search arrives with the Bleve stream (positions).

---

## 2. Current state

`SEARCH BODY`/`TEXT` already **work**, but via a brute-force scan: the session
loads every message in the folder and, for any criteria that needs the body,
fetches the **entire raw message** and matches it with go-imap's
`MatchMessage`:

- Handler: `session.Search` — `internal/imap/server.go:2157`.
- `needsBody` decision — `internal/imap/server.go:2176`.
- Per-message `Fetch` + `MatchMessage` — `internal/imap/server.go:2190`.

FTS replaces this scan with an index lookup returning candidate UIDs,
intersected with the remaining (flag / date / seq) criteria. The scan remains
as the fallback — nothing regresses.

---

## 3. Reference analysis — the 2.4 FTS model and its engine's limits

### 3.1 How the reference framework works (facts the design reproduces)

- **Checkpoint**: the last indexed UID plus a settings checksum live in a
  per-mailbox index-extension header. The checksum detects tokenizer/language
  config drift and forces a rebuild.
- **No expunge journal**: expunges reach the FTS index synchronously via the
  storage sync-notification hook of whatever FTS-enabled process next syncs
  the mailbox, with `rescan` as the reconciliation safety net.
- **Indexing pipeline**: MIME walk → decode to UTF-8 → per-part decision
  (text parts indexed; `multipart/*` skipped; binary parts only via a
  decoder/parser such as Tika); headers filtered by include/exclude (default:
  index unless excluded); address headers normalized; body size capped by
  `fts_message_max_size`.
- **Tokenization**: a generic word tokenizer (`simple` algorithm, token max
  length 30 bytes) plus an address tokenizer (max 250) that emits whole
  e-mail addresses as single tokens; a filter chain (lowercase, snowball,
  stopwords, ICU normalizer) that is **empty by default** in the reference.
- **Query symmetry**: at query time every word is expanded to
  `OR{raw, tokenized-unfiltered, filtered}`; a multi-word string becomes an
  `AND` of its tokens; stopwords are dropped from the query. Index-time and
  query-time may use *different tokenizers* but share the same filter chain.
- **On-demand indexing at SEARCH**: a first-missing-UID check with one
  refresh-and-retry, a priority insert into the indexer queue, and a wait
  bounded by `fts_search_timeout`.
- **Autoindex throttle**: `fts_autoindex_max_recent_msgs` — skip autoindex
  when a mailbox has too many `\Recent` messages, protecting against
  mass-delivery reindex storms.
- **Relevancy**: engine scores flow through per-level AND(max-on-common) /
  OR(union-max) merges and surface only via a relevancy fetch special.
- Multi-mailbox lookup exists **only for virtual mailboxes** — not for
  ordinary cross-mailbox search. (yarilo has no virtual mailboxes yet; noted
  as future.)

### 3.2 Reference Xapian-engine limitations that we will not inherit

| # | Limitation |
|:--|:--|
| L1 | **No phrase search** — no positional data is stored; a multi-word query silently degrades to `AND` of terms → false positives the client sees as wrong matches |
| L2 | **Prefix-only matching** by default (right-truncation wildcard) — RFC 3501 requires substring; the substring mode stores *all suffixes* of every token (index bloat, off by default) |
| L3 | **Over-approximation on non-indexed headers** — matched only in the pooled all-headers prefix; one such arg in an AND makes *all* results "maybe" → the core re-reads every candidate message |
| L4 | **Silent index loss on lock timeout** — the writer lock retries 60×1 s, then the update is dropped with only a log line, unindexed until a rescan |
| L5 | **Expunge scans every shard** to find the UID; **rescan deletes everything above the lowest missing UID** (reindex storm) |
| L6 | **Blocking optimize** — full compaction holds write handles on all shards; a manual single-threaded rebuild fallback; runs at session teardown |
| L7 | **Scores effectively unused** — raw unnormalized weights, only via a nonstandard fetch path |
| L8 | **No stemming out of the box** — the default language filter chain is empty |
| L9 | Misc: 200-byte term cap, a first-character lowercase workaround (Xapian treats a leading capital as a term prefix), per-mailbox DB × shard proliferation, no-fsync writes (crash ⇒ corrupt shard ⇒ rescan storm) |

---

## 4. Architecture

```
  APPEND / LMTP deliver            EXPUNGE (any session)
        |                                |
        v                                v
  +--------------------------- session pods ---------------------------+
  |  imap / lmtp: INDEX (autoindex, write-through at delivery),        |
  |  EXPUNGE, LOOKUP, PREPEND (on-demand catch-up at SEARCH)           |
  +---------------------------------------------------------------------+
        |            TAB-delimited protocol, LF, version handshake
        v
  +--------------------------+
  |       yarilo-fts         |   sole owner of the index files:
  |  queue + worker + lookup |   single writer, in-process readers
  |  engine: flatcurve|bleve |   (embedded mode: linked into the test
  +--------------------------+    binary — the yarilo-locks pattern)
        |
        v
  engine-defined index layout under the mailbox index root (§7)
```

**Why a service owns lookups too.** Bleve/scorch is strictly single-process:
one writer plus snapshot readers *within the same process*; two processes must
not open the same index, and mmap+locks on NFS are unreliable. yarilo runs
several session pods over a shared RWX volume, so in-process reads from
session pods are off the table. Instead, `yarilo-fts` is the sole process that
opens the index (an LRU of open per-user indexes), and sessions send `LOOKUP`
over the wire. This simultaneously fixes L4 (a real queue instead of blind
lock retries — nothing is silently dropped) and removes cross-process locking
from the hot path entirely. `pkg/locks` still guards the index directory as
the project rule requires (protects against a second `yarilo-fts` instance
and rescan/optimize races).

Modes, per the `yarilo-locks` precedent (config-not-binary):

- **embedded** — the engine linked in-process; unit tests and single-process
  CLI runs only.
- **remote** — a `yarilo-fts` Deployment; every k8s topology. Scale-out beyond
  one replica is by user-hash sharding (same consistent-hash family as the
  director) — documented as a follow-up, replicas=1 initially.

---

## 5. Engine interface (`pkg/fts`)

Modelled on the reference backend vtable, adapted to the per-user index
model:

```go
type Engine interface {
    Name() string
    OpenUser(user UserRef) (UserIndex, error)   // one index per user
}

type UserIndex interface {
    // Checkpoint per mailbox (folder GUID): survives restarts, detects config drift.
    Checkpoint(mbox MailboxRef) (lastIndexedUID uint32, settingsChecksum uint32, err error)
    SetCheckpoint(mbox MailboxRef, lastUID, checksum uint32) error

    BeginUpdate(mbox MailboxRef) (Update, error)
    Expunge(mbox MailboxRef, uid uint32) error
    Rescan(mbox MailboxRef, present []uint32) (missing []uint32, err error)  // targeted diff, §8
    Optimize() error
    Refresh() error

    Lookup(mbox MailboxRef, q Query) (Result, error)
    Close() error
}

type Update interface {
    SetBuildKey(k BuildKey) (accept bool, err error)
    BuildMore(utf8 []byte) error   // valid UTF-8, not NUL-terminated
    Commit() error
    Rollback() error
}
```

**Build-key model**: `KeyHeader{HdrName}`, `KeyMIMEHeader`,
`KeyBodyPart{ContentType}`, `KeyBodyPartBinary`, each carrying the UID — a
clean split of header fields from body and attachments that gives fielded
search for free.

**Query** carries the already-expanded token tree (§6): per-word
`OR{raw, unfiltered, filtered}` variants, `AND` across words, plus a native
**phrase** node the Bleve engine executes as a position-aware phrase query
(the flatcurve engine degrades it to `AND` — the shard format has no
positions).

**Result** — `Definite` UIDs, `Maybe` UIDs (candidates needing
re-verification against the raw message), and normalized per-query
`Scores{uid → float}` with AND=max-on-common / OR=union-max merge semantics.

Engine capability flags: `Tokenized`, `Positions` (phrase-capable),
`Substring`, `Scoring`, `BinaryMIMEParts` — the core adapts query building and
the indexing stream per engine.

Implemented in `pkg/fts` (PR #580).

---

## 6. Text extraction, tokenization, query symmetry

**Extraction** (`internal/fts/buildmail`): MIME walk via `go-message`; text
parts and `message/*` indexed; `multipart/*` containers skipped; HTML → text
(script/style/head subtrees dropped); base64 runs (≥ 50 chars with
leader/trailer rules) skipped; `fts_message_max_size` cap; headers filtered by
`fts_header_includes`/`excludes` (default: index unless excluded; a trailing
`*` is a prefix mask; an include match overrides an exclude match); address
headers decoded before indexing. Attachment decoding (Tika / script socket)
is Phase 3, behind the same `fts_decoder_*` knobs.

**Tokenization** (`internal/fts/language`): generic word tokenizer (token max
30 bytes, apostrophe continuation, multibyte-safe truncation) + address
tokenizer (max 250, whole address as one token; at index time the parts also
flow through the word tokenizer, at search time the address is withheld so a
query matches only the whole-address token); filter chain =
lowercase → stopwords → snowball stemmer — **enabled by default** (an
improvement over the reference's empty default chain, L8); 7 snowball
languages; CJK via a bigram analyzer (Bleve stream). Configurable per
`language`/`language_filters` keys.

**Query symmetry**: the search string runs through the *search* tokenizer +
the same filter chain; each word expands to `OR{the whole original string,
the unfiltered token, the filtered token}`; stopwords are dropped (the word
was never indexed, so it must not constrain the query); multi-word strings
become `AND` of words **plus**, on position-capable engines, a phrase query so
`SEARCH BODY "foo bar"` matches the actual phrase (fixes L1). A
`fts_search_strict` knob (default off) additionally verifies candidates
against the raw message for exact RFC 3501 substring semantics (bounded work:
candidates only, never the whole folder — fixes L2 without suffix bloat).

Implemented in `internal/fts/language` + `internal/fts/buildmail` (PR #580).

---

## 7. Storage layout — engine-defined, under the mailbox index root

The on-disk layout belongs to the engine; both live under the existing index
resolution — `UserInfo.IndexDir` (the `INDEX=` override) → `MailPath` → `Home`
(`internal/storage/index/file/file.go:365-380`) — configurable and
FS-agnostic.

**flatcurve (first engine, PR #581):** a `fts-flatcurve/` directory per
**mailbox** under that mailbox's index dir, holding `current.###` /
`index.###` Xapian shards, docid == UID, term prefixes `A`/`H<NAME>`/`B`,
and yarilo's own shard version key (`yarilo.fts-flatcurve`). There is no
direct in-place migration from other installations — indexes are rebuilt by
the indexer — so no cross-product on-disk compatibility promise is carried.

The engine opens shards with Xapian `DB_NO_SYNC`, so the wrapper creates and
**fsyncs each `current.###` directory (and the rename on rotate) itself**
before handing the path to Xapian — otherwise the directory entry stays
unflushed and a write can race into a not-yet-durable shard (the "Couldn't
write new rev file … (No such file or directory)" wedge of #629). On **any**
engine error the write handle is released (`discardCurrent`) so the next
update reopens a fresh shard instead of returning a poisoned handle forever;
the `yarilo-fts` worker additionally evicts and reopens the whole user handle
when it sees a `DatabaseClosedError`-class fault, so a wedged index self-heals
without an operator deleting `fts-flatcurve/` on disk.

**Bleve (follow-up stream):** one index per **user**:

```
<index-root>/yarilo-fts/          # one Bleve (scorch) index per user
```

- Document ID = `<folder-guid>:<uid>`; survives folder renames; UIDVALIDITY
  change invalidates only that mailbox's documents.
- Fields: `body`/`subject` with positions (phrase-capable); dynamic
  `hdr_<name>` per header (no pooled-prefix "maybe" class, L3); `mailbox`
  filter field; no stored content.
- Background segment merging instead of blocking optimize (L6); append-only
  segments + snapshots for crash consistency (vs no-fsync corruption, L9);
  index size gated by the benchmark (§14).

**Engine-independent (framework level, applies to both):**

- **Per-mailbox checkpoint** (`last_indexed_uid`, `uidvalidity`, `settings_checksum`)
  in the engine metadata (flatcurve on-disk file format `2 <uidvalidity> <last_uid>
  <settings_checksum>`, tolerant of the legacy v1 `1 <last_uid> <settings_checksum>`
  which reads `uidvalidity` back as 0). A **checksum mismatch** rebuilds the mailbox
  (tokenizer/filter config changed); a **UIDVALIDITY mismatch** means the mailbox was
  recreated, so the checkpoint is stale and its `last_indexed_uid` can sit above the
  new low UIDs — the indexer detects this and rebuilds from scratch rather than
  silently skipping every new message (#638). The reference gets this for free by
  co-locating the fts header inside the mailbox's own index (recreated on
  UIDVALIDITY change); yarilo's `yarilo-fts` is a separate process that must not write
  the session-owned fileindex, so it tracks UIDVALIDITY explicitly in its own
  checkpoint. The checkpoint read-modify-write runs under the per-mailbox lock so
  concurrent index jobs for one mailbox can't clobber each other's progress.
- Every write path in `yarilo-fts` takes the user's mailbox lock via
  `pkg/locks` (project rule) around update/rescan/optimize sessions.

---

## 8. Consistency model (expunge, rescan, config drift)

Mirrors the reference sync-driven model (no expunge journal) with targeted
improvements:

- **Online expunge**: session pods send `EXPUNGE <user> <folder-guid> <uid>`
  at the same call sites that emit `EventExpunged` today
  (`internal/imap/server.go:549`, LMTP/sieve discard paths). Bleve: point
  delete by document ID. flatcurve: the writer keeps a per-shard UID-range
  map in memory, avoiding an open-every-shard probe (softens L5-expunge even
  on the shard layout).
- **Offline reconciliation**: `RESCAN` diffs the index against the mailbox —
  the engine API takes the *present UID set*, deletes exactly the stale
  documents and reports exactly the missing UIDs. No "delete everything above
  the lowest missing UID" storm (fixes L5-rescan).
- **Config drift**: `settings_checksum` over the tokenizer/filter/header
  config in the per-mailbox checkpoint; mismatch ⇒ that mailbox is rebuilt.
- **Queue durability**: index requests are queued; a failure surfaces as a
  checkpoint gap, which the next autoindex/on-demand pass heals. Nothing is
  silently dropped on contention — there is no lock retry-then-give-up path
  at all (fixes L4).

---

## 9. Engines

### 9.1 Xapian / flatcurve — first engine (cgo, FTS-1; PR #581)

Follows the flatcurve on-disk layout (per-**mailbox** DBs,
`current.###`/`index.###` shards, prefixes `A`/`H`/`B`, docid == UID) with
yarilo's own shard version key (`yarilo.fts-flatcurve`) — there is no direct
in-place migration path from other installations (indexes are rebuilt by
the indexer), so no cross-product compatibility promise is carried. Runs
inside the `yarilo-fts` service behind the engine interface; keeps the
`fts_flatcurve_*` settings verbatim (`commit_limit` 500, `min_term_size` 2,
`optimize_limit` 10, `rotate_count` 5000, `rotate_time` 5000 ms,
`substring_search` no; the 200-byte term cap is a format constant, not a
setting).

**Build implication:** Xapian is C++, so `yarilo-fts` builds with
`CGO_ENABLED=1` + libxapian. Because the service is the *only* process that
touches the index (§4), the cgo dependency is confined to this one binary —
all session binaries keep the pure-Go static build inside the same single
image. There are no maintained Go bindings for Xapian, so the
engine ships its own minimal C shim (~250 lines) covering exactly the calls
it needs.

Even on this shard layout, the framework carries improvements the reference
engine lacks: queued writes (no silent drops, L4), targeted rescan diff (L5),
a per-shard UID-range map for expunge (§8), write-through delivery indexing
(§10), and stemming on by default (§6, L8).

### 9.2 Bleve v2 / scorch — separate follow-up stream (pure Go)

Deferred to its own stream (see §14): positions on body/subject (phrase
queries), BM25 scoring, background merging, snapshot rollback, one index per
user — plus its own `fts_bleve_*` keys (including a positions knob if the
size benchmark warrants one). Keeps `CGO_ENABLED=0`. bluge was evaluated and
rejected (development stalled; Bleve v2 is actively maintained).

---

## 10. The `yarilo-fts` service

TAB-delimited, LF-terminated protocol with version handshake (internal-protocol
style). Requests:

```
INDEX   <user> <folder-guid> <max-uid> <max-recent>   # autoindex / catch-up
PREPEND <user> <folder-guid> <max-uid>                # priority (on-demand search)
EXPUNGE <user> <folder-guid> <uid>
LOOKUP  <user> <query-wire>                           # returns UIDs (+scores) per folder
RESCAN  <user> [<folder-guid>]
OPTIMIZE <user>
STATUS  <user> <folder-guid>                          # last_indexed_uid
```

- **Worker loop** (INDEX): read checkpoint → walk
  `lastIndexedUID+1 .. max-uid` → fetch message → `buildmail` → engine update
  stream → batch commit (default 500) → advance checkpoint. Two distinct
  failure classes get deliberately different handling (#721): a **fetch**
  failure (unreadable/vanished file) is tolerated — skip, log, advance the
  checkpoint past it, flag the folder for a reactive heal — "one unreadable
  message must not stall the mailbox forever". A **build** failure
  (`buildmail.Build` itself errors — a hard decoder failure per #697, or a
  genuinely unparseable message) is NOT tolerated the same way: the
  in-progress engine document for that UID is rolled back (it would
  otherwise sit half-built until the NEXT message's first build-key flushes
  it into the shard — the exact bug #721 fixes), whatever was already fully
  built before it is committed and checkpointed, and the run halts without
  advancing past the failed UID — so a later index run naturally retries it
  (e.g. once a decoder config issue is fixed) instead of it being silently,
  permanently skipped. This deliberately changes behaviour for a genuinely
  unparseable top-level message, which used to be tolerated like a fetch
  error — see #721. Every halt logs at Error and increments
  `fts_index_build_halts_total`, since a deterministic per-document failure
  keeps halting on the same UID every retry until fixed and must stay
  visible, not scroll by once and go quiet.
- **Write-through at delivery** (improvement): LMTP hands the *already
  in-memory* message to `INDEX` as an inline payload variant, so
  delivery-time indexing does not re-read the message from storage.
- **On-demand at SEARCH**: the session computes the first missing UID from
  `STATUS` vs `uidnext`, sends `PREPEND`, polls bounded by
  `fts_search_timeout` (30 s default; 250 ms poll).
- **Autoindex**: on `EventDelivered` when `fts_autoindex: true`, throttled by
  `fts_autoindex_max_recent_msgs` (skip when the folder's recent backlog
  exceeds the limit).
- **Manual**: `yarctl fts rescan|optimize|status` → backend-api →
  service.
- **Automatic optimize** (#715): the flatcurve engine implements the
  optional `fts.OptimizeNotifier` capability — right after a shard
  rotation, if the mailbox's sealed-shard count has reached
  `fts_flatcurve_optimize_limit`, it calls back into the service (a fast,
  non-blocking enqueue only, never compaction work) to push the mailbox
  onto a dedicated `optimizeQueue`, deduped by `user+mailbox GUID` so
  repeated rotations at/above the limit don't pile up duplicate work. A
  single background goroutine drains that queue, calling the engine's
  per-mailbox `OptimizeMailbox` (as opposed to whole-user `Optimize`) so
  one large mailbox's compaction never blocks indexing of a user's other
  mailboxes. Both paths — manual whole-user `Optimize` and the
  per-mailbox auto-optimize — serialize on the same per-user lock the
  engine already requires for correctness, so no separate coordination is
  needed to avoid compacting the same directory twice at once; `optimizeDir`
  is also a cheap no-op below 2 shards, so an occasional redundant call
  costs nothing. `fts_flatcurve_optimize_limit: 0` disables auto-optimize
  entirely (manual only). A leftover `optimize` compaction tmp dir from a
  crash mid-run is swept lazily, the first time that mailbox's directory is
  opened (the service has no upfront list of every mailbox to sweep at
  process start). Observability: `slog.Info` per completed optimize run
  (user, folder, shards merged, duration) plus `fts_optimize_runs_total` /
  `fts_optimize_shards_merged_total`.
- **Time-based rotation** (#724): `fts_flatcurve_rotate_time` was the same
  dead-knob class as #715 — accepted by config, defaulted, never read.
  `commitCurrent` now measures its own commit's wall-clock duration and, if
  it exceeded `RotateTime`, rotates right after — the reference's own
  shape: `rotate_count` alone never catches a mailbox with few, large
  documents whose single commit takes a long time. This is centralized in
  `commitCurrent` itself (via a `*Engine` back-reference on `mboxState`)
  rather than threaded through every call site (the write path's
  `CommitLimit` check, an explicit `Commit()`, `Refresh()`), so the
  time-based trigger applies uniformly regardless of what triggered the
  commit; a rotation from this path also runs the same
  `notifyOptimizeIfNeeded` (#715) a count-based rotation does, since both
  seal a shard. `fts_flatcurve_rotate_time: 0` disables the time-based
  trigger (`rotate_count`-only), the same "0 = special, not silently
  defaulted" convention `fts_flatcurve_optimize_limit` uses.

---

## 11. SEARCH integration

In `session.Search` (`internal/imap/server.go:2157`):

1. FTS enabled and criteria contain `Body`/`Text`/`Header` → build the
   expanded query (§6) → `LOOKUP`.
2. Intersect `Definite` UIDs with non-FTS criteria (flags, dates, seq/UID,
   size, modseq) evaluated cheaply from the index metadata.
3. Re-verify `Maybe` UIDs (flatcurve engine only) and, in
   `fts_search_strict` mode, definite candidates too — by fetching only those
   messages.
4. Unindexed tail + `fts_search_add_missing` allows → `PREPEND` + bounded
   wait; on timeout behave per `fts_search_read_fallback`. The wait also
   **gives up early when the checkpoint makes no progress** (a broken FTS
   backend keeps it flat): ~2s of no movement falls back to the scan rather
   than blocking the full timeout, so a wedged index never surfaces as a
   client-visible TCP hang (#629). A genuinely-progressing index keeps the
   full window.
5. FTS off / error / fallback → the existing sequential scan (unchanged
   behaviour). The reference defaults its read-fallback to *off*; we default
   **true** because our scan already exists and is correct — no regression by
   default.
6. Scores → session score map → `SEARCH RETURN (RELEVANCY)` / relevancy fetch
   special (Phase 2), with AND=max-on-common / OR=union-max merges.

`OR` / `NOT` composition: sub-args the engine can't resolve are evaluated by
the core; the engine flags what it handled so the core does not needlessly
re-scan.

---

## 12. Where this is better than the reference stack

The *When* column says which stream delivers the axis: **FTS-1** (framework +
flatcurve engine) or **Bleve** (the follow-up engine stream).

| Axis | Reference engine | yarilo FTS | When |
|:--|:--|:--|:--|
| Contention behaviour | 60×1 s lock retry, then silent drop (L4) | queued service, nothing dropped | FTS-1 |
| Rescan | reindex storm above lowest gap (L5) | targeted diff of exact UIDs | FTS-1 |
| Expunge | opens every shard to find the UID (L5) | per-shard UID-range map (flatcurve) / point delete by doc ID (Bleve) | FTS-1 / Bleve |
| Stemming default | off (L8) | snowball + stopwords on | FTS-1 |
| Substring / RFC 3501 | prefix-only, or suffix-bloat mode (L2) | prefix by default + `fts_search_strict` candidate verification | FTS-2 |
| Delivery-time indexing | re-reads message from disk | write-through: indexes the in-memory message | FTS-1 |
| Phrase search | none (L1), false positives | positional phrase queries on body/subject | Bleve |
| Arbitrary-header search | pooled → maybe → re-read all candidates (L3) | per-header fields, definite results | Bleve |
| Optimize | blocking full compaction (L6) | background segment merging | Bleve |
| Crash safety | no-fsync → corrupt shard (L9) | append-only segments + snapshots, checkpoint replay | Bleve |
| Relevancy | raw weights, unused (L7) | normalized BM25 → RELEVANCY | FTS-2 ✅ |
| DB proliferation | dirs × shards per mailbox (L9) | one index per user | Bleve |

Known trade-offs (tracked): single-writer service is a throughput bottleneck
per user (acceptable for mail; user-hash sharding is the scale-out path);
language/normalization coverage below full ICU (snowball set; ICU normalizer
optional later); Bleve index size vs Xapian glass is measured by the FTS-1
benchmark before the Bleve stream starts (positions cost; mitigated by
limiting them to body/subject).

---

## 13. Configuration (`fts:` section) + Helm

```yaml
fts:
  ## Master switch + explicit engine selection. fts_engine is REQUIRED when
  ## enabled: there is no implicit default — startup fails fast on a missing
  ## or unknown engine name, so the active engine is always stated in config.
  enabled: false
  fts_engine: ""                    # "flatcurve" (FTS-1) | "bleve" (arrives with
                                    # its own stream, together with fts_bleve_* keys)

  ## Service topology (yarilo-locks precedent).
  fts_mode: remote                  # remote (k8s) | embedded (tests/CLI)
  fts_addr: ""                      # e.g. "yarilo-fts:9106"
  fts_max_conns: 4                  # connections per session process — see below

  ## Indexing behaviour.
  fts_autoindex: false
  fts_autoindex_max_recent_msgs: 0
  fts_message_max_size: 0           # 0 = unlimited
  fts_header_includes: []
  fts_header_excludes: []
  fts_commit_limit: 500             # batch size, any engine

  ## Search behaviour.
  fts_search_add_missing: body-search-only
  fts_search_read_fallback: true    # see §11 — our scan exists, default safe
  fts_search_timeout: 30s
  fts_search_strict: false          # RFC substring verification of candidates
  fts_search: true                  # false = SEARCH-only degrade, indexing keeps running (#726)

  ## Language chain.
  language_filters: [lowercase, stopwords, snowball]   # default ON
  languages: [en]                   # >1 enables per-part detection (#696)
  fts_language_filters_override: {} # per-language override, e.g. {uk: [lowercase, stopwords]} (#726)
  fts_detection_sample_bytes: 0     # 0 = default 1024; bytes sampled per part
  fts_detection_min_runes: 0        # 0 = default 10; reliability threshold
  fts_language_tokenizer_generic_token_maxlen: 0   # 0 = default 30 (#726)
  fts_language_tokenizer_address_token_maxlen: 0   # 0 = default 250 (#726)
  fts_language_tokenizer_generic_algorithm: simple # tr29 errors at startup, not yet implemented (#726)
  fts_language_tokenizer_generic_wb5a: false        # TR29-only, errors if true (#726)
  fts_language_tokenizer_generic_explicit_prefix: false # TR29-only, errors if true (#726)

  ## Decoder (Phase 3).
  fts_decoder_driver: ""            # "" | script | tika
  fts_decoder_script_socket_path: ""
  fts_decoder_tika_url: ""
  fts_decoder_max_attempts: 0       # tika retry against network/5xx; 0 = default 2

  ## Engine-specific: flatcurve (fts_engine: "flatcurve"; the yarilo-fts
  ## binary links libxapian — cgo confined to the fts Deployment image).
  ## fts_bleve_* keys arrive with the Bleve stream.
  fts_flatcurve_commit_limit: 500
  fts_flatcurve_min_term_size: 2
  fts_flatcurve_optimize_limit: 10  # auto-queues a mailbox at this shard count (#715); 0 disables
  fts_flatcurve_rotate_count: 5000
  fts_flatcurve_rotate_time: 5000  # ms; also rotates on a slow commit (#724); 0 disables
  fts_flatcurve_substring_search: false
```

Helm: `components.fts` Deployment (replicas 1; ClusterIP `:9106`; the index
volume). `appVersion` bump ships with each feature slice.

### The index queue

The queue holds **at most one pending pass per mailbox**. A mailbox is the unit
of work, not a request: a pass reads the checkpoint and indexes everything above
it, so two passes over one mailbox do the same work twice — and the second finds
that out only after taking the lock. Under delivery load those duplicates are
what turn a busy mailbox into a stream of contention, which is why coalescing is
a correctness property here rather than an optimisation.

Merging keeps the widest range: the pass covers the highest UID any request
asked for, and the strictest `max_recent` bound. A priority request (a search
catch-up) for a mailbox already queued **moves** it to the front instead of
adding a second entry.

A request that arrives while its mailbox is being indexed is not dropped: the
running pass read its checkpoint before that request existed and will not cover
it, so the mailbox is re-queued when the pass finishes. Every popped mailbox is
released through the same path whatever happens to the pass — including a panic
— because a mailbox left claimed would never be queued again, and its mail would
stay unindexed with nothing in the log to say so.

### Reading ahead (`fts_prefetch_depth`, `fts_prefetch_max_bytes`)

An index pass can read messages ahead of the one it is tokenising, so storage
reads overlap with parsing. **It is off by default**, and that is a measurement
rather than caution:

```
fts_build_seconds  115.5 ms/message
fts_read_seconds     0.34 ms/message   ← 0.3% of a pass
```

Tokenising and base64 decoding dominate; the disk barely appears. Overlapping
reads with parsing therefore recovers almost nothing, while the read-ahead
window holds messages in memory once per worker.

Raise it where reads are genuinely slow — cold alt-tier storage, or an NFS mount
whose cache is not warm. `fts_read_seconds` against `fts_build_seconds` is what
says whether that is your deployment; do not raise it on the assumption that it
must help.

When enabled, four properties hold:

**Advisory.** A depth below two reads one message at a time — the same code
path, not a second one.

**Order is mandatory.** Indexing walks UIDs upwards and the checkpoint advances
monotonically, so a pipeline that reordered would record progress past messages
it never indexed. One producer feeding one channel makes that impossible rather
than unlikely.

**A read failure stays attached to its message**, so a pass skips a message it
can name instead of halting on one it cannot.

**Two ceilings.** Depth bounds messages, `fts_prefetch_max_bytes` bounds what
they hold — and the byte ceiling is per pass, so N workers can hold N windows. A
message larger than the whole ceiling is admitted alone: refusing to index it
would be worse than briefly exceeding a bound whose purpose is limiting
concurrency.

### Dispatch: parallelism is across users (`fts_index_workers`)

`fts_index_workers` is how many mailboxes are indexed at once. What it can and
cannot buy follows from the engine: `userIndex` holds **one mutex per user**,
taken by every write. Two workers on two mailboxes of the same user therefore
serialise inside the engine — they would occupy two workers to do one user's
work while other users wait.

So dispatch hands each worker a **different user**: a worker takes the
highest-priority mailbox whose user is not already being indexed, and skips the
rest. Nothing is lost — a skipped mailbox is dispatched as soon as its user is
released — and one user with many mailboxes cannot occupy every worker.

A consequence worth recognising before it is diagnosed as a stall: workers can
sit idle while the queue is deep, because everything queued belongs to users
already running. `fts_pop_skipped_total` tells those two apart, and
`rate(fts_worker_busy_seconds_total[5m])` shows how much of the worker budget is
actually in use.

That is a counter rather than a gauge on purpose. A pass takes tens of
milliseconds and a scrape happens every 15-30 seconds, so a gauge of "workers
busy now" is sampled between passes almost every time: it reads zero whether the
service is saturated or idle. A metric that cannot answer the question it was
added for is worse than none, because the question then looks answered.

The useful lever is not this number. Indexing is CPU-bound — `fts_build_seconds`
is almost all of a pass — so worker count is bounded by the CPU the container is
given, and `components.fts.resources.limits.cpu` defaults to `1`. A second
worker under a one-CPU limit adds scheduling and a second prefetch window, not
throughput. Raise the CPU limit and the worker count together, or add pods.

### Locking: the full-text index is not the mail index

An index pass takes a lock keyed `fts:<user>:<folder>`, not the mailbox lock.
The two are different resources with different writers: the mail index is
written by every session, the full-text index only by `yarilo-fts`. The lock
exists solely so two full-text passes over one mailbox — after a ring move, for
instance — cannot race the checkpoint's read-modify-write.

Sharing the mailbox key made every pass queue behind unrelated session writes.
Under load that was not theoretical: the lock is taken without waiting, so a
busy mailbox meant an immediate failure, and the pass was dropped rather than
retried (#1004). The mail-index reads a pass needs (`OpenFolder`,
`GetMessages`, the present-UID snapshot) deliberately happen **outside** the
lock, so nothing was relying on the wider scope.

A pass that still loses its lock — genuine contention between two full-text
writers — is requeued with a bounded backoff rather than dropped, and counted
in `fts_index_deferred_total`. A pass abandoned after exhausting its retries
increments `fts_index_dropped_total`, which is expected to stay at zero: every
increment is mail that is not in the index and will not be until something else
queues that mailbox.

### Telemetry

| Metric | Answers |
|:---|:---|
| `fts_index_lag_uids` | how much mail is not in the index — the largest gap between a mailbox's highest UID and its checkpoint |
| `fts_index_lag_seconds` | how old the oldest unindexed message is |
| `fts_index_queue_depth` | mailboxes waiting for a pass |
| `fts_queue_wait_seconds` | how long they waited — separates "deep queue" from "deep queue that is not draining" |
| `fts_queue_merged_total` | requests folded into a queued pass: duplicate work, and duplicate lock contention, that did not happen |
| `fts_queue_requeued_total` | mailboxes put back because a request arrived mid-pass |
| `fts_fetch_seconds` | opening a message — **not** reading it |
| `fts_read_seconds` | time actually blocked reading a message's bytes |
| `fts_build_seconds` | the whole of indexing one message |
| `fts_build_stage_seconds{stage}` | where that time went: `parse`, `decode`, `tokenize`, `write` |
| `fts_prefetch_inflight_bytes` | bytes read ahead and not yet indexed |
| `fts_prefetch_stall_seconds` | the reader waiting for window space — growth means the ceiling is the bottleneck, not the disk |
| `fts_message_bytes` | the size those two are relative to; 200ms on 30MB and on 3KB are different diagnoses |
| `fts_index_deferred_total` | passes requeued after losing the lock |
| `fts_index_dropped_total` | passes given up on. **Expected zero** |
| `fts_worker_busy_seconds_total` | total time workers spent inside passes; `rate()` divided by `fts_index_workers` is utilisation |
| `fts_pop_skipped_total` | mailboxes passed over because their user was already being indexed — this is what separates "idle because there is no work" from "idle because the work belongs to busy users" |

`fts_fetch_seconds` timed only the open for a long time while the body streamed
into the tokeniser, so every byte of storage I/O was being counted as parsing —
which made the read share look like 7% of a pass when the real figure was
unknown. `fts_read_seconds` is what settles that.

`fts_build_seconds` answers "is indexing CPU-bound" — it is, overwhelmingly.
`fts_build_stage_seconds` answers the next question, which is what the CPU is
doing:

- `parse` — reading the MIME structure;
- `decode` — extracting text from attachments, including anything handed to an
  external decoder;
- `write` — feeding terms to the engine;
- `tokenize` — the remainder.

Those first three are timed directly. Tokenising is **derived**: reading a text
part is interleaved with tokenising it, because the reader feeds the tokeniser
through a callback, and separating them would require every layer to subtract
its children. So `tokenize` is the pass time that is not parsing, decoding or
writing — accurate as a share, not as an isolated measurement, and named here
rather than left to look measured.

The split matters because the fixes differ. Decoding an attachment that yields
no useful terms and tokenising a body cost the same in the total, and one is a
question about `fts_message_max_size` and the decoder settings — what is worth
indexing at all — while the other is a question about the tokeniser.

The two lag gauges are the ones a user feels: "search does not find recent
mail". Everything else is a proxy for them.

They carry no per-user or per-mailbox labels — a few hundred users times their
folders would put that cardinality into Prometheus. The per-mailbox detail is
kept in process and only the worst case is published, sampled every 30 seconds;
for a specific user, use the logs.

### Connections to the service (`fts_max_conns`)

How many connections a session process keeps to `yarilo-fts`. It matters more
than the name suggests: **one connection serialises request and response
pairs**, so a caller that issues several lookups at once gets them queued, not
run in parallel. Until this is above one, a search fanning out over several
folders is sequential however the caller is written.

The service has always handled each connection on its own goroutine — the
client side was the bottleneck. Connections open on demand, so a pool of four
costs nothing until four calls actually overlap.

A caller that finds every connection busy waits up to the dial timeout and then
gets a distinct "no free connection" error, so an operator can tell "the service
is busy" from "the service failed". Setting it to `1` reproduces the previous
single-connection behaviour exactly.

### Migration notes: legacy config → `yarilo.yaml` (#727)

Config keys are named to track the reference's own `fts_flatcurve_*` /
`fts_*` naming closely, but a few values and shapes need translation, not a
literal copy:

- **`fts_message_max_size`**: yarilo's `0` means unlimited. The reference
  rejects a literal `0` at startup and spells "no limit" as the keyword
  `unlimited` — copying a numeric `0` across from a reference config that
  actually meant "reject anything over 0 bytes" (not the common case, but
  possible) would silently flip its meaning; copying the reference's
  `unlimited` keyword across as literal text would fail to parse here.
  Translate `unlimited` → `0`, and any other reference value across as
  the same byte count.
- **Time values**: the reference uses TIME strings (`"30s"`, `"5s"`, …).
  yarilo uses bare integers with the unit fixed in the key name —
  `fts_search_timeout_secs` (seconds), `fts_flatcurve_rotate_time`
  (milliseconds). Strip the unit suffix and convert if the reference value
  used a different unit than the yarilo key expects (e.g. reference
  `"5s"` → yarilo `fts_flatcurve_rotate_time: 5000`, not `5`).
- **`fts_decoder_driver`**: yarilo's "disabled" value is the literal string
  `none`; the reference uses an empty string. An empty string here is
  also accepted as "disabled" (see `decoder.New`), so this is forgiving,
  but `none` is the canonical yarilo spelling going forward.
- **`fts_decoder_script_addr`**: a superset of the reference's plain
  socket-path value — accepts `unix:///path/to.sock` (a bare reference
  socket path becomes `unix://` + that same path) or `host:port` for a
  standalone Deployment/Service the reference has no equivalent topology
  for.

### Known divergences (#727)

Deliberate differences from the reference, kept because the tradeoff was
evaluated and accepted — not gaps to close silently:

- **Subject is indexed via the no-stemming data chain** (#696), unlike the
  reference, which stems Subject/Comments/Keywords like body text.
  Prefix-wildcard search (flatcurve's own `Xapian::Query(OP_WILDCARD, ...)`
  shape, unconditional on every term) compensates when the query word's
  stem happens to be a PREFIX of the indexed unstemmed word (e.g. English
  `run` as a prefix of indexed `running`) — but not when a language's
  stemming rule doesn't produce a prefix relationship (e.g. Russian
  `бежать`/`бегу` share no common prefix with their stem; German
  compounding can move the stem to the middle of the surface form). A
  Subject search in those languages may miss an inflected form the
  reference's stemmed index would have caught.
- **No ICU normalizer yet**: no diacritics folding, so `café` does not
  match a search for `cafe`. Tracked on the FTS-2 roadmap (§14), not
  started.
- **Mid-token `*` tokenizes differently** (#725 item 4's own
  investigation): the reference treats `*` as a token-continuation
  character in its generic tokenizer (not a break), so `foo*bar` is one
  token there; yarilo's tokenizer treats `*` as a break character, so
  `foo*bar` becomes two tokens (`foo`, `bar`). No practical recall impact
  — if anything, treating `*` as a break is the softer behavior (it can
  only ever match MORE, never fewer, real-world messages containing a
  literal asterisk mid-word) — kept as a known, accepted divergence rather
  than fixed, since replicating it exactly would need `*` to stop being a
  break character everywhere, which has its own tokenization
  consequences elsewhere in the break table.
- **RELEVANCY excludes score-less UIDs from normalization** (`internal/imap/fts.go`'s
  `relevancyScores`): a UID matched by a stripped non-FTS criterion but
  absent from the engine's own score map is floored at `1`, not folded
  into the min-max range via an implicit `0.0` the way the reference's
  plain map lookup would — that implicit zero would otherwise drag the
  whole result set's low end down and compress every genuine score toward
  100. An improvement over the reference's own behavior, not a gap.
- **Rescan does targeted deletes against the exact missing-UID list**
  (`internal/fts/flatcurve/engine.go`'s `Rescan`), rather than the
  reference's delete-everything-above-the-lowest-missing-UID reindex
  storm. Also an improvement, already tracked in §12's comparison table
  (Rescan row) — repeated here since it's the kind of divergence an
  operator diffing behavior against the reference might otherwise flag
  as a bug.

---

## 14. Phases

1. **FTS-1** (in progress): `pkg/fts` interface ✅ (PR #580) +
   **flatcurve engine** ✅ (PR #581; Xapian cgo confined to the `yarilo-fts`
   binary) + `internal/fts/buildmail` ✅ + `internal/fts/language` ✅
   (stemming on by default) + the `yarilo-fts` service (queue, worker,
   LOOKUP, wire protocol, embedded/remote) + expunge/rescan consistency +
   SEARCH integration with fallback + write-through LMTP indexing +
   config/Helm + `yarctl fts` + tests + **index-size and
   search-latency benchmark** ✅ (acceptance gate — `internal/ftsbench`
   + `app/fts-bench`).
2. **FTS-2**: relevancy surface (`SEARCH RETURN (RELEVANCY)`) ✅ — engine
   `MSetEntry.Weight` → `fts.Result.Scores` → min-max normalized 1-100 per
   RFC 4731/6203, parsed/encoded via a `yarilo-patches` fork of go-imap
   (upstream has no RELEVANCY support at all) + `fts_search_strict` ✅
   (already wired pre-#668, verified in review) + multi-language detection
   ✅ — deliberately ASYMMETRIC design verified against the reference
   implementation: indexing auto-detects one language per body/attachment
   PART (`internal/fts/language.MultiChain.SelectForIndex`, called per part
   by `buildmail`, falling back to the first configured `languages` entry
   on a short/ambiguous sample after one bounded retry with a larger
   sample), while search expands every query token through EVERY
   configured language's stemmer, OR'd together as extra `fts.Word`
   variants (`MultiChain.ExpandSearch`) — "enough for one of them to
   match" without knowing which language a given part was detected as.
   Detection via `github.com/abadojack/whatlanggo`, restricted to the
   configured `languages` set. Stopword lists (Snowball project) added for
   all 7 stemmed languages (en/fr/de/it/pt/ru/es — previously only `en` had
   one, which would have hard-errored any other language's `stopwords`
   filter). Header values (addresses, message-ids, subjects) are indexed
   through a dedicated no-stemming "data" chain — lowercase only, never a
   detected language — since search already matches them via the raw
   query-token variant `ExpandSearch` always includes (#696, refining the
   original per-message design from #668 point 3: per-part detection so a
   quoted reply or attachment in a different language from the rest of the
   message indexes correctly, plus tunable `fts_detection_sample_bytes` /
   `fts_detection_min_runes`). This changes indexed tokens vs. the original
   per-message design, so `detectionAlgoVersion` is mixed into
   `MultiChain.SettingsChecksum()` to force existing mailboxes through the
   settings-drift rebuild path. ICU normalizer option (not started).
   **Stemmer-less languages ✅ (#718)**: `uk` (Ukrainian) is configurable —
   there is no official Snowball algorithm for it at all (the reference
   implementation cannot stem it either), so the `snowball` filter step
   becomes a no-op passthrough for any language absent from
   `snowballStemmers` rather than a hard chain-construction error; a
   stemmer-less language is then defined purely by its other filters
   (`uk` = lowercase + stopwords, via a canonical open-source list —
   `internal/fts/language/data/stopwords_uk.txt`, MIT-licensed
   stopwords-iso). `uk` is also added to `isoToWhatlang` so per-part
   detection can classify it — critical for `languages: [uk, ru]`, since
   without detection a Ukrainian part would be indexed under whichever
   language happens to be `languages[0]`, and Russian's stemmer applied to
   Ukrainian text produces wrong stems. `Chain.SettingsChecksum()` already
   mixes in `Language`, so a `uk` chain's checksum differs from `ru`'s with
   no further change needed.
   **Stopword-only SEARCH ✅ (#722)**: a Body/Text/Header criterion whose
   every token expands to nothing (pure stopwords — never indexed) used to
   be silently dropped as "no constraint", so e.g. `SEARCH BODY "the"`
   matched the ENTIRE mailbox instead of nothing. Fixed at both the
   per-criterion level (`buildFTSQuery`'s per-arg expansion now marks the
   whole query `impossible`, not just drops that one criterion) and at
   `prepareFTSSearch` (an impossible query returns an empty covered set,
   not `allUIDs`), matching the reference implementation's own
   fts-search-args.c: an empty expansion becomes match-nothing, and since
   criteria are ANDed, one unmatchable criterion makes the whole query
   unmatchable regardless of any other criteria that DID expand to real
   terms. This is a definite answer (bypasses `ftsCatchUp`/
   `fts_search_read_fallback` entirely, same as a real Lookup result), not
   an index-unavailable condition.
   **Header SEARCH no longer stemmed ✅ (#723, search-side of #696)**:
   `buildFTSQuery` expanded HEADER criteria through the full configured
   language chain, but buildmail indexes every header value through the
   no-stemming data chain (#696) — the mismatch meant a query like `HEADER
   Message-Id running` added a stemmed `run` variant, which became a
   flatcurve wildcard (`Hmessage-idrun*`) matching an unrelated indexed
   word sharing the same prefix (e.g. "runway"), as a false DEFINITE hit
   with no re-verify when `fts_search_strict=false`. `language.NewDataChain()`
   is now the single shared constructor both buildmail (`New`) and
   `internal/imap` (`headerDataChain`) call, so index-time and query-time
   header tokens are guaranteed identical — HEADER criteria route through
   it uniformly (no per-field exception; Subject included, since we don't
   stem it at index time either).
   **Tokenizer/build small divergences batch ✅ (#725)**: found by the
   post-#715 audit, seven items fixed against the reference:
   1. an address with an empty domain (`user@`) was emitted as a phantom
      whole-address token — `Address.emitAddress` now requires `@` not be
      the last byte after trimming;
   2. a trailing `-` (a valid mid-domain atext byte) wasn't trimmed off the
      end of a collected domain the way trailing `.` already was — same
      fix, `TrimRight(addr, ".-")`;
   3. the fullwidth apostrophe U+FF07 wasn't recognized by `isApostrophe`
      alongside ASCII `'` and U+2019, so e.g. `don＇t` split into `don`+`t`;
   4. investigated and **not fixed**: the reference strips a trailing `*`
      from a token because `*` isn't a break character for it, but in our
      tokenizer `*` already IS a break character (`asciiWordBreaks`), so
      `foo*` already tokenizes to `foo` — the trailing case is a non-issue
      here. A narrower residual divergence (`foo*bar` → one token in the
      reference, `foo`+`bar` for us) is cosmetic (no recall impact, if
      anything softer) and tracked as a known divergence in #727, not
      fixed in this PR;
   5. the header NAME itself wasn't indexed at all (only its value) — the
      reference feeds the name into the A-pool so `SEARCH TEXT "list-id"`
      matches by header name. `buildHeaders` now issues a SEPARATE build
      key with an empty `HdrName` for the name (A-pool only, per
      `BuildMore`'s `name == ""` guard) — not the same key as the value,
      which would otherwise make `HEADER List-Id "list-id"` spuriously
      match its own name;
   6. the header-existence boolean term (`B<name>`) was set unconditionally
      in `SetBuildKey`, even when the value produced zero indexable tokens
      — the reference only records existence together with at least one
      real (`>=min_term_size`) token. Moved to `BuildMore`, set lazily on
      the first real token per field, so `HEADER X-Foo ""` (or any
      all-too-short value) no longer spuriously matches;
   7. address headers (From/To/Cc/Bcc/Reply-To/Sender) tokenized the
      already-RFC2047-decoded text as one blob instead of structured
      address-list parsing. `addressHeaderText` now calls
      `net/mail.ParseAddressList` on the RAW (still-encoded) bytes — the
      reference's own order: decoding RFC2047 BEFORE parsing can turn
      decoded display-name characters (`(`, `[`, `<`) into RFC 5322
      comment/special delimiters, corrupting the parse. `net/mail` decodes
      the encoded-word phrase as part of parsing the raw bytes, so
      parse-raw-then-decode happens in one call; a parse failure falls back
      to tokenizing the decoded text as-is.

   Items 5 and 7 change indexed header tokens, so `detectionAlgoVersion`
   (mixed into `MultiChain.SettingsChecksum()`) bumped 2→3 to force
   existing mailboxes through the settings-drift rebuild path.
   **Config parity ✅ (#726)**: four gaps between what's configurable in
   the reference and what was hard-coded here.
   1. `fts_language_tokenizer_generic_token_maxlen` /
      `fts_language_tokenizer_address_token_maxlen` (0 = defaults 30/250)
      now reach `NewMultiChain` instead of being hard-coded `0, 0` at both
      call sites (`yarilo-fts` indexing, session-side query expansion).
   2. `fts_language_tokenizer_generic_algorithm` accepts `simple`
      (implemented, default) and `tr29` — `tr29` (and its TR29-only
      companions `fts_language_tokenizer_generic_wb5a` /
      `..._explicit_prefix`) reject at startup with a clear error via
      `language.ValidateTokenizerConfig`, rather than silently falling
      back to `simple` or no-op'ing. Investigated what `explicit_prefix`
      actually controls (trailing-`*` prefix-search semantics, #725 item
      4's own investigation): flatcurve already prefix-matches every term
      unconditionally (its own `Xapian::Query(OP_WILDCARD, ...)` shape),
      so the knob would have no visible effect even implemented today —
      real value only appears with a future exact-match backend (Bleve).
      Deliberately not implemented now; tracked for the Bleve stream.
   3. **`fts_search`** (default `true`, #726 item 3): disables SEARCH only
      — `FTSOptions.SearchEnabled` gates `prepareFTSSearch`'s sole
      `enabled()` check, degrading every SEARCH to the sequential scan as
      if FTS weren't configured. Indexing/autoindex/write-through don't
      check this flag at all, so they keep running — an incident-response
      knob ("the FTS index/engine is misbehaving, stop querying it,
      don't lose freshness") distinct from `fts.enabled` (all-or-nothing).
   4. **Per-language filter override** (`fts_language_filters_override`,
      map, #726 item 4): a language absent from the map uses the global
      `language_filters` unchanged; a present language's list is a full
      replacement, not a merge (e.g. `uk` without `snowball` even though
      the global default has it, for a mixed `uk`/`ru` deployment).
      `NewMultiChain` validates every override key names a configured
      language (typos like `ukr` are a startup error, not silently
      ignored). The override's mere PRESENCE — not just its resolved
      effect on filters — is mixed into `MultiChain.SettingsChecksum()`,
      so adding/removing an override (even to a list identical to the
      global default) forces a reindex for that language.
3. **FTS-3**: attachment decoders (script / Tika, #669) + within-message
   attachment text dedup by content hash ✅. Refined by #697: the `script`
   driver caches an optional `TYPES` supported-types/extensions list
   (queried once, on its own connection) so unsupported parts are skipped
   locally instead of dialing DECODE and shipping the attachment bytes for
   nothing — a decoder that doesn't recognize `TYPES` (ERROR response,
   connection close, or a read timeout — all three tested) falls back to
   asking every part, unchanged from before. The `tika` driver now sends
   the filename via `Content-Disposition`, retries connection errors/5xx
   (bounded, `fts_decoder_max_attempts`) instead of erroring immediately,
   and treats 204 as explicitly empty alongside 415/422. A decoder error
   is now classified: retries exhausted against a transient condition
   (`decoder.ErrDegraded`) indexes the message without that attachment's
   text (`fts_decoder_degraded_total`) rather than looping it through
   autoindex forever for a failure that won't self-heal; any other error
   (bad config, a permanent 4xx, a script protocol error) aborts the
   message so autoindex retries it later — previously every decoder error
   was silently swallowed as a permanent skip, which for a transient Tika
   outage meant the attachment text was never indexed even after Tika
   recovered.
4. **Bleve stream** (separate stream, own issue): Bleve v2/scorch engine —
   per-user index, positional phrase search, background merging, crash-safe
   segments — plus its `fts_bleve_*` keys (including a positions knob if the
   size benchmark warrants one) and the default-engine decision revisited
   with benchmark numbers.

---

## 15. Testing

- **Unit**: buildmail (MIME → keys, include/exclude, HTML, base64 skip, size
  cap); tokenizer/filters (maxlen 30/250, address tokens, stopword drop);
  query expansion (raw/unfiltered/filtered OR, phrase AND + phrase node);
  engine (checkpoints, point expunge, targeted rescan, UIDVALIDITY change);
  score merge (AND=max-on-common/OR=union-max).
- **Service**: protocol round-trip, PREPEND priority, on-demand wait +
  timeout, autoindex throttle, write-through payload, queue behaviour under
  contention (nothing dropped).
- **Integration (`internal/imap`)**: APPEND → SEARCH BODY finds; EXPUNGE →
  gone; header-field search; fallback correctness with FTS off/erroring;
  multi-pod expunge reconciliation via rescan.
- **Benchmarks (acceptance)**: index size vs corpus size; index throughput;
  SEARCH latency indexed vs brute-force; NFS behaviour of the service-owned
  index.
- **Live sandbox**: deploy `yarilo-fts`, imaptest, manual SEARCH BODY/TEXT,
  `yarctl fts` flows.

---

## 16. References

yarilo anchors: `internal/imap/server.go:2157/2176/2190/549`;
`pkg/mailbox/interfaces.go:137`; `internal/storage/index/file/file.go:365-380`;
`pkg/config/config.go`; `internal/backend/backend.go`; `pkg/mailbox/path.go:21`.

Implementation: `pkg/fts` (engine contract, score merges),
`internal/fts/language` (tokenizers, filters, query expansion),
`internal/fts/buildmail` (MIME → build keys),
`internal/fts/flatcurve` (Xapian engine + cgo shim).

External: Xapian (<https://xapian.org/docs/>), Bleve v2
(<https://github.com/blevesearch/bleve>).
