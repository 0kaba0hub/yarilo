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
lowercase → snowball stemmer → stopwords — **enabled by default** (an
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
  co-locating the fts header inside the mailbox's own `dovecot.index` (recreated on
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

TAB-delimited, LF-terminated protocol with version handshake (INTERNALS.md
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
  stream → batch commit (default 500) → advance checkpoint.
- **Write-through at delivery** (improvement): LMTP hands the *already
  in-memory* message to `INDEX` as an inline payload variant, so
  delivery-time indexing does not re-read the message from storage.
- **On-demand at SEARCH**: the session computes the first missing UID from
  `STATUS` vs `uidnext`, sends `PREPEND`, polls bounded by
  `fts_search_timeout` (30 s default; 250 ms poll).
- **Autoindex**: on `EventDelivered` when `fts_autoindex: true`, throttled by
  `fts_autoindex_max_recent_msgs` (skip when the folder's recent backlog
  exceeds the limit).
- **Manual**: `yarilo-admin fts rescan|optimize|status` → backend-api →
  service.

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

  ## Language chain.
  language_filters: [lowercase, snowball, stopwords]   # default ON
  languages: [en]                   # >1 enables per-part detection (#696)
  fts_detection_sample_bytes: 0     # 0 = default 1024; bytes sampled per part
  fts_detection_min_runes: 0        # 0 = default 10; reliability threshold

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
  fts_flatcurve_optimize_limit: 10
  fts_flatcurve_rotate_count: 5000
  fts_flatcurve_rotate_time: 5000
  fts_flatcurve_substring_search: false
```

Helm: `components.fts` Deployment (replicas 1; ClusterIP `:9106`; the index
volume). `appVersion` bump ships with each feature slice.

---

## 14. Phases

1. **FTS-1** (in progress): `pkg/fts` interface ✅ (PR #580) +
   **flatcurve engine** ✅ (PR #581; Xapian cgo confined to the `yarilo-fts`
   binary) + `internal/fts/buildmail` ✅ + `internal/fts/language` ✅
   (stemming on by default) + the `yarilo-fts` service (queue, worker,
   LOOKUP, wire protocol, embedded/remote) + expunge/rescan consistency +
   SEARCH integration with fallback + write-through LMTP indexing +
   config/Helm + `yarilo-admin fts` + tests + **index-size and
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
  `yarilo-admin fts` flows.

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
