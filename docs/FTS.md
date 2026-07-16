# Full-text search (FTS) — design

Status: **design / plan, revision 3** (issue #250, Phase FTS-1). No code yet —
this document is the plan of record and is reviewed before implementation.

Full-text indexing of message bodies and headers so that IMAP `SEARCH BODY`,
`SEARCH TEXT` and `SEARCH HEADER` are answered from an index instead of a
brute-force scan.

**Bar:** not worse than Dovecot 2.4 + fts-flatcurve on any axis, and measurably
better on the axes listed in §12. The design is grounded in a full read of the
local Dovecot 2.4 source (`src/plugins/fts`, `src/plugins/fts-flatcurve`,
`src/indexer`, `src/lib-language`) including a limitations audit of flatcurve
itself; file:line references are given inline.

---

## 1. Goals

- Answer `SEARCH BODY` / `TEXT` / `HEADER <field>` from a full-text index;
  fall back to the existing sequential scan when the index is unavailable or a
  lookup fails.
- **Pluggable engines** behind one interface. The **first implemented engine is
  `flatcurve`** (Xapian, byte-compatible with Dovecot's on-disk format — the
  deliverable issue #250 asks for, and an immediate migration path). A pure-Go
  engine (Bleve v2 / scorch) is a **separate follow-up stream** (§14) that
  plugs into the same interface and carries the position-dependent
  improvements (phrase search) plus its own `fts_bleve_*` keys.
- **A dedicated `yarilo-fts` service** that owns the indexes — sole writer *and*
  the lookup endpoint — with asynchronous indexing, on-demand catch-up at
  search time, autoindex on delivery, and manual rescan/optimize. Embedded mode
  for tests and single-process CLI runs, remote mode in every k8s deployment —
  the exact `yarilo-locks` precedent. A welcome consequence: **cgo/libxapian is
  confined to the single `yarilo-fts` binary** — every session binary stays
  pure Go; only the fts Deployment uses the cgo image.
- Dovecot-compatible configuration key names where they exist, per the
  parity rule.
- RFC-correct substring behaviour where flatcurve is not (see §3); RFC-correct
  phrase search arrives with the Bleve stream (positions).

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
as the fallback — nothing regresses. There is no existing FTS code in the tree.

---

## 3. Reference analysis — Dovecot 2.4 model and flatcurve's limits

### 3.1 What Dovecot 2.4 does (facts the design must reproduce)

- **Checkpoint**: `last_indexed_uid` + `settings_checksum` live in a mail-index
  extension header named `fts` (`fts-api-private.h:105`, `fts-api.c:539-624`).
  The checksum detects tokenizer/language config drift and forces a rebuild.
- **No expunge journal**: the 2.3 expunge-log is gone. Expunges reach the index
  synchronously via the storage `sync_notify` hook of whatever FTS-enabled
  process next syncs the mailbox (`fts-storage.c:691-719`), with `rescan` as
  the reconciliation safety net (`fts-api.c:361-377`).
- **Indexing pipeline** (`fts-build-mail.c`): MIME walk → decode to UTF-8 →
  per-part decision (text parts indexed; `multipart/*` skipped; binary parts
  only via a decoder/parser such as Tika); headers filtered by
  include/exclude (default: index unless excluded); address headers
  normalized; body size capped by `fts_message_max_size`.
- **Tokenization** (`lib-language`): generic tokenizer (`simple` algorithm,
  token max length 30), address tokenizer (max 250) that emits whole e-mail
  addresses as single tokens; filter chain (lowercase, snowball, stopwords,
  ICU normalizer) — **empty by default** (`lang-settings.c:36`).
- **Query symmetry** (`fts-search-args.c:56-190`): at query time every word is
  expanded to `OR{raw, tokenized-unfiltered, filtered}`; a multi-word string
  becomes an `AND` of its tokens; stopwords are dropped from the query.
  Index-time and query-time may use *different tokenizers* but share the same
  filter chain.
- **On-demand indexing at SEARCH**: first-missing-UID check with one
  refresh-and-retry (`fts-search.c:341-367`), `PREPEND` priority insert into
  the `indexer` queue, wait bounded by `fts_search_timeout`.
- **Autoindex throttle**: `fts_autoindex_max_recent_msgs` — skip autoindex when
  a mailbox has too many `\Recent` messages (`master-connection.c:257-268`),
  protecting against mass-delivery reindex storms.
- **Relevancy**: backend scores flow through per-level AND(min)/OR(max) merges
  (`fts-search.c:229-339`) and surface only via the `RELEVANCY` fetch special
  (`fts-storage.c:451-475`).
- `lookup_multi` is used **only for virtual mailboxes** — not for ordinary
  cross-mailbox search.

### 3.2 flatcurve limitations (from its own source) that we will not inherit

| # | Limitation | Evidence |
|:--|:--|:--|
| L1 | **No phrase search** — no positional data (`add_term` only); multi-word queries are silently degraded to `AND` of terms → false positives | `fts-backend-flatcurve-xapian.cc:1720-1766, 2141-2153` |
| L2 | **Prefix-only matching** by default (`OP_WILDCARD` right-truncation) — RFC 3501 requires substring; substring mode stores *all suffixes* of every token (index bloat, off by default) | `xapian.cc:2018-2054, 1704-1731`; `fts-flatcurve-settings.c:30` |
| L3 | **`maybe_uids` degradation** — search on a non-indexed header matches only the pooled `A` prefix; one maybe arg in an AND makes *all* results maybe → core re-reads every candidate message | `xapian.cc:2049-2066, 2314-2320` |
| L4 | **Silent index loss on lock timeout** — writer lock retries 60×1 s, then the update is dropped with only a log line, unindexed until a rescan | `xapian.cc:103-110, 412-443`; `fts-backend-flatcurve.c:264-296` |
| L5 | **Expunge scans every shard** to find the UID; **rescan deletes everything above the lowest missing UID** (reindex storm) | `xapian.cc:1224-1282`; `fts-backend-flatcurve.c:457-462` |
| L6 | **Blocking optimize** — full compaction holds write handles on all shards; manual single-threaded rebuild fallback; runs at session deinit | `xapian.cc:1793-1877` |
| L7 | **Scores effectively unused** — raw unnormalized weights, only via nonstandard fetch path | `xapian.cc:2269`; `fts-backend-flatcurve.c:622-711` |
| L8 | **No stemming out of the box** — default `language` filter chain is empty | `lang-settings.c:36` |
| L9 | Misc: 200-byte term cap, first-char lowercase hack (Xapian prefix collision), per-mailbox DB × shard proliferation, `DB_NO_SYNC` writes (crash ⇒ corrupt shard ⇒ rescan storm) | `fts-backend-flatcurve.c:16,352`; `xapian.cc:457, 819-872, 1710-1727` |

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
  per-USER index  <index-root>/yarilo-fts/   (one index per user, §7)
```

**Why a service owns lookups too (design change vs revision 1).** Bleve/scorch
is strictly single-process: one writer plus snapshot readers *within the same
process*; two processes must not open the same index, and mmap+locks on NFS
are unreliable (the Lucene story). yarilo runs several session pods over a
shared RWX volume, so in-process reads from session pods are off the table.
Instead, `yarilo-fts` is the sole process that opens the index (an LRU of open
per-user indexes), and sessions send `LOOKUP` over the wire. This
simultaneously fixes flatcurve's L4 (a real queue instead of blind lock
retries — nothing is silently dropped) and removes cross-process locking from
the hot path entirely. `pkg/locks` still guards the index directory as the
project rule requires (protects against a second `yarilo-fts` instance and
rescan/optimize races).

Modes, per the `yarilo-locks` precedent (config-not-binary):

- **embedded** — the engine linked in-process; unit tests and single-process
  CLI runs only.
- **remote** — a `yarilo-fts` Deployment; every k8s topology. Scale-out beyond
  one replica is by user-hash sharding (same consistent-hash family as the
  director) — documented as a follow-up, replicas=1 initially.

---

## 5. Engine interface (`pkg/fts`)

Isomorphic to Dovecot's `struct fts_backend_vfuncs`
(`fts-api-private.h:12-56`), adapted to the per-user index model:

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
    Expunge(mbox MailboxRef, uid uint32) error   // point delete, no shard scan
    Rescan(mbox MailboxRef, present []uint32) error  // targeted diff, §8
    Optimize() error                             // hint only for scorch (§7)
    Refresh() error

    Lookup(q Query) (Result, error)
    Close() error
}

type Update interface {
    SetBuildKey(k BuildKey) (accept bool, err error)
    BuildMore(utf8 []byte) error   // valid UTF-8, not NUL-terminated
    Commit() error
}
```

**Build-key model** — copied from Dovecot (`fts-api.h:29-45`):
`KeyHeader{HdrName}`, `KeyMIMEHeader`, `KeyBodyPart{ContentType}`,
`KeyBodyPartBinary`, each carrying the UID.

**Query** carries the already-expanded token tree (§6): per-word
`OR{raw, unfiltered, filtered}` variants, `AND` across words, plus a native
**phrase** node the Bleve engine executes as a position-aware phrase query
(the flatcurve engine degrades it to `AND`, as upstream does).

**Result** — `DefiniteUIDs` (grouped per mailbox), `MaybeUIDs`, and normalized
per-query `Scores{uid → float}` with AND=min / OR=max merge semantics matching
`fts-search.c:229-339`. With per-header fields (§7) the Bleve engine produces
no maybes; the class exists for the flatcurve engine's pooled-header matches.

Engine capability flags (analogue of `fts_backend_flags`): `Tokenized`,
`Positions` (phrase-capable), `Substring`, `Scoring`, `BinaryMIMEParts` — the
core adapts query building per engine.

---

## 6. Text extraction, tokenization, query symmetry

**Extraction** (`internal/fts/buildmail`), mirroring `fts-build-mail.c`: MIME
walk via `go-message`; text parts and `message/*` indexed; `multipart/*`
containers skipped; HTML → text; base64 runs (≥ 50 chars) skipped;
`fts_message_max_size` cap; headers filtered by
`fts_header_includes`/`excludes` (default: index unless excluded); address
headers (From/To/Cc/…) normalized before indexing. Attachment decoding
(Tika / script socket) is Phase 3, behind the same `fts_decoder_*` knobs.

**Tokenization** (`internal/fts/language`): generic word tokenizer (token max
30) + address tokenizer (max 250, whole address as one token) as in
`lib-language`; filter chain = lowercase → snowball stemmer → stopwords —
**enabled by default** (improvement over Dovecot's empty default chain, L8);
Unicode segmentation via UAX#29 (`blevesearch/segment`); CJK via the bigram
analyzer. Configurable per `language`/`language_filters` keys.

**Query symmetry** — reproduced exactly from `fts-search-args.c`: the search
string runs through the *search* tokenizer + the same filter chain; each word
expands to `OR{raw, unfiltered, filtered}`; stopwords are dropped; multi-word
strings become `AND` of words **plus**, on position-capable engines, a phrase
query so `SEARCH BODY "foo bar"` matches the actual phrase (RFC-correct,
fixes L1). A `fts_search_strict` knob (default off, matching Dovecot
behaviour) additionally verifies candidates against the raw message for exact
RFC 3501 substring semantics (bounded work: candidates only, never the whole
folder — fixes L2 without suffix bloat).

---

## 7. Storage layout — engine-defined, under the mailbox index root

The on-disk layout belongs to the engine; both live under the existing index
resolution — `UserInfo.IndexDir` (`INDEX=` override) → `MailPath` → `Home`
(`internal/storage/index/file/file.go:365-380`) — configurable and
FS-agnostic, as in Dovecot (`MAILBOX_LIST_PATH_TYPE_INDEX`).

**flatcurve (first engine):** byte-compatible with upstream — a
`fts-flatcurve/` directory per **mailbox** under that mailbox's index dir,
holding `current.###` / `index.###` Xapian shards, docid == UID, term
prefixes `A`/`H`/`B`. Existing Dovecot flatcurve indexes are readable as-is.

**Bleve (follow-up stream):** one index per **user**:

```
<index-root>/yarilo-fts/          # one Bleve (scorch) index per user
```

- Document ID = `<folder-guid>:<uid>`; survives folder renames; UIDVALIDITY
  change invalidates only that mailbox's documents.
- Fields: `body`/`subject` with positions (phrase-capable); dynamic
  `hdr_<name>` per header (no pooled-`A`/maybe class, L3); `mailbox` filter
  field; no stored content.
- Background segment merging instead of blocking optimize (L6); append-only
  segments + snapshots for crash consistency (vs `DB_NO_SYNC`, L9); index
  size gated by the benchmark (§14).

**Engine-independent (framework level, applies to both):**

- **Per-mailbox checkpoint** (`last_indexed_uid`, `settings_checksum`) in the
  engine metadata — the analogue of Dovecot's `fts` index-header extension;
  checksum mismatch ⇒ rebuild of that mailbox.
- Every write path in `yarilo-fts` takes the user's mailbox lock via
  `pkg/locks` (project rule) around update/rescan/optimize sessions.

---

## 8. Consistency model (expunge, rescan, config drift)

Mirrors 2.4 (no expunge journal) with targeted improvements:

- **Online expunge**: session pods send `EXPUNGE <user> <folder-guid> <uid>`
  at the same call sites that emit `EventExpunged` today
  (`internal/imap/server.go:549`, LMTP/sieve discard paths). Bleve: point
  delete by document ID. flatcurve: our writer keeps a per-shard UID-range
  map in memory, avoiding upstream's open-every-shard probe (softens
  L5-expunge even on the compatible format).
- **Offline reconciliation**: `RESCAN` diffs the index against the mailbox
  (the worker walks the folder's UID list) and — because our engine API takes
  the *present UID set* — deletes exactly the stale documents and indexes
  exactly the missing UIDs. No "delete everything above the lowest missing
  UID" storm (fixes L5-rescan).
- **Config drift**: `settings_checksum` over the tokenizer/filter/header
  config in the per-mailbox checkpoint; mismatch ⇒ that mailbox is rebuilt
  (matches `fts_index_have_compatible_settings`, `fts-api.c:593-624`).
- **Queue durability**: index requests are enqueued (in-memory + retry on
  failure surfaces in the checkpoint gap, which the next autoindex/on-demand
  pass heals). Nothing is silently dropped on contention — there is no lock
  retry-then-give-up path at all (fixes L4).

---

## 9. Engines

### 9.1 Xapian / flatcurve — first engine (cgo, FTS-1)

Byte-compatible with Dovecot flatcurve on-disk format (per-**mailbox** DBs,
`current.###`/`index.###` shards, prefixes `A`/`H`/`B`, docid == UID,
`flatcurve-lock`) — existing Dovecot installations migrate by pointing yarilo
at the same index root. Runs inside the `yarilo-fts` service behind the
engine interface; inherits upstream's `fts_flatcurve_*` settings verbatim
(`commit_limit` 500, `min_term_size` 2, `optimize_limit` 10, `rotate_count`
5000, `rotate_time` 5000 ms, `substring_search` no; term cap 200 bytes is a
constant upstream, not a setting).

**Build implication:** Xapian is C++, so `yarilo-fts` builds with
`CGO_ENABLED=1` + libxapian. Because the service is the *only* process that
touches the index (§4), the cgo dependency is confined to this one binary —
all session binaries keep the pure-Go static build; only the fts Deployment
uses the cgo image.

Even on the compatible format, the framework carries improvements upstream
lacks: queued writes (no silent drops, L4), targeted rescan diff (L5), a
per-shard UID-range map for expunge (§8), write-through delivery indexing
(§10), and stemming on by default (§6, L8).

### 9.2 Bleve v2 / scorch — separate follow-up stream (pure Go)

Deferred to its own stream (see §14): positions on body/subject (phrase
queries), BM25 scoring, background merging, snapshot rollback, one index per
user — plus its own `fts_bleve_*` keys (including the positions knob if the
size benchmark warrants one). Keeps `CGO_ENABLED=0`. bluge was evaluated and
rejected (development stalled; Bleve v2 is actively maintained by Couchbase).

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
  stream → batch commit (default 500) → advance checkpoint. Mirrors
  `index_mailbox_precache` (`master-connection.c:67`).
- **Write-through at delivery** (improvement): LMTP hands the *already
  in-memory* message to `INDEX` as an inline payload variant, so delivery-time
  indexing does not re-read the message from storage. Flatcurve cannot do
  this architecturally.
- **On-demand at SEARCH**: session computes first-missing-UID from `STATUS`
  vs `uidnext`, sends `PREPEND`, polls bounded by `fts_search_timeout` (30 s
  default; 250 ms poll — `fts-indexer.c` semantics).
- **Autoindex**: on `EventDelivered` when `fts_autoindex: true`, throttled by
  `fts_autoindex_max_recent_msgs` exactly as upstream (skip when the folder's
  recent backlog exceeds the limit).
- **Manual**: `yarilo-admin fts rescan|optimize|status` → backend-api →
  service.

---

## 11. SEARCH integration

In `session.Search` (`internal/imap/server.go:2157`):

1. FTS enabled and criteria contain `Body`/`Text`/`Header` → build the
   expanded query (§6) → `LOOKUP`.
2. Intersect `DefiniteUIDs` with non-FTS criteria (flags, dates, seq/UID,
   size, modseq) evaluated cheaply from the index metadata.
3. Re-verify `MaybeUIDs` (flatcurve engine only) and, in
   `fts_search_strict` mode, definite candidates too — by fetching only those
   messages.
4. Unindexed tail + `fts_search_add_missing` allows → `PREPEND` + bounded
   wait; on timeout behave per `fts_search_read_fallback`.
5. FTS off / error / fallback → existing sequential scan (unchanged
   behaviour). Note upstream 2.4 defaults `read_fallback` to *false* in the
   base settings; we default **true** because our scan already exists and is
   correct — no regression by default.
6. Scores → session score map → `SEARCH RETURN (RELEVANCY)` /
   relevancy fetch special (Phase 2), with AND=min / OR=max merges.

---

## 12. Where this is better than Dovecot 2.4 + flatcurve

The *When* column says which stream delivers the axis: **FTS-1** (framework +
flatcurve engine) or **Bleve** (the follow-up engine stream).

| Axis | flatcurve upstream | yarilo FTS | When |
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
| Crash safety | `DB_NO_SYNC` → corrupt shard (L9) | append-only segments + snapshots, checkpoint replay | Bleve |
| Relevancy | raw weights, unused (L7) | normalized BM25 → RELEVANCY | FTS-2 |
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
  fts_search_read_fallback: true    # upstream base default is false; see §11
  fts_search_timeout: 30s
  fts_search_strict: false          # RFC substring verification of candidates

  ## Language chain.
  language_filters: [lowercase, snowball, stopwords]   # default ON
  languages: [en]                   # >1 enables per-body detection (later phase)

  ## Decoder (Phase 3).
  fts_decoder_driver: ""            # "" | script | tika
  fts_decoder_script_socket_path: ""
  fts_decoder_tika_url: ""

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

1. **FTS-1** (first code iteration): `pkg/fts` interface +
   **flatcurve engine** (Xapian cgo, byte-compatible; cgo confined to the
   `yarilo-fts` binary) + `internal/fts/buildmail` + `internal/fts/language`
   (stemming on by default) + the `yarilo-fts` service (queue, worker,
   LOOKUP, wire protocol, embedded/remote) + expunge/rescan consistency +
   SEARCH integration with fallback + write-through LMTP indexing +
   config/Helm + `yarilo-admin fts` + tests + **index-size and
   search-latency benchmark** (acceptance gate).
2. **FTS-2**: relevancy surface (`SEARCH RETURN (RELEVANCY)` / fetch special),
   `fts_search_strict`, multi-language detection, ICU normalizer option.
3. **FTS-3**: attachment decoders (script / Tika) + attachment text dedup by
   content hash.
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
  score merge (AND=min/OR=max).
- **Service**: protocol round-trip, PREPEND priority, on-demand wait +
  timeout, autoindex throttle, write-through payload, queue behaviour under
  contention (nothing dropped).
- **Integration (`internal/imap`)**: APPEND → SEARCH BODY finds; EXPUNGE →
  gone; header-field
  search; fallback correctness with FTS off/erroring; multi-pod expunge
  reconciliation via rescan.
- **Benchmarks (acceptance)**: index size vs corpus size; index throughput;
  SEARCH latency indexed vs brute-force; NFS behaviour of the service-owned
  index.
- **Live sandbox**: deploy `yarilo-fts`, imaptest, manual SEARCH BODY/TEXT,
  `yarilo-admin fts` flows.

---

## 16. References

Dovecot 2.4 source (local, primary): `src/plugins/fts/fts-api.h`,
`fts-api-private.h`, `fts-api.c`, `fts-build-mail.c`, `fts-search.c`,
`fts-search-args.c`, `fts-indexer.c`, `fts-storage.c`, `fts-settings.c`;
`src/plugins/fts-flatcurve/*`; `src/indexer/*`; `src/lib-language/*`.

yarilo anchors: `internal/imap/server.go:2157/2176/2190/549`;
`pkg/mailbox/interfaces.go:137`; `internal/storage/index/file/file.go:365-380`;
`pkg/config/config.go`; `internal/backend/backend.go`; `pkg/mailbox/path.go:21`.

Supplementary: Bleve v2 (<https://github.com/blevesearch/bleve>, scorch,
query packages), Bleve multi-process constraints (upstream discussion),
<https://doc.dovecot.org/main/core/plugins/fts.html>,
<https://doc.dovecot.org/main/core/plugins/fts_flatcurve.html>,
<https://xapian.org/docs/>.
