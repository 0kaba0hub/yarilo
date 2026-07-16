# Full-text search (FTS) — design

Status: **design / plan** (issue #250, Phase FTS-1). No code yet — this document
is the plan of record and is reviewed before implementation begins.

Full-text indexing of message bodies and headers so that IMAP `SEARCH BODY`,
`SEARCH TEXT` and `SEARCH HEADER` are answered from an index instead of a
brute-force scan.

---

## 1. Goals

- Answer `SEARCH BODY` / `TEXT` / `HEADER <field>` from a per-mailbox full-text
  index; fall back to the existing sequential scan when the index is missing,
  disabled, or a lookup fails.
- **Pluggable engines** behind one interface: a pure-Go engine (Bleve) as the
  default, and a Xapian-backed `flatcurve`-compatible engine as an opt-in.
- **Asynchronous indexing** driven by a dedicated `yarilo-fts-indexer` worker,
  plus on-demand catch-up when a search touches unindexed mail (mirrors
  Dovecot's `indexer` / `indexer-worker` split).
- Dovecot-compatible configuration key names so a `dovecot.conf` → `yarilo.yaml`
  migration stays mechanical, per the project's Dovecot-parity rule.

Modelled on Dovecot 2.4's `fts` framework (`src/plugins/fts`), the
`fts-flatcurve` plugin (`src/plugins/fts-flatcurve`), and the `indexer` services
(`src/indexer`). File:line references to the Dovecot source are given inline so
the implementation can be checked against the reference.

---

## 2. Current state

`SEARCH BODY`/`TEXT` already **work**, but via a brute-force scan: the session
loads every message in the folder and, for any criteria that needs the body
(`BODY`, `TEXT`, `HEADER`, sent-date), fetches the **entire raw message** from
storage and matches it with go-imap's `MatchMessage`:

- Handler: `session.Search` — `internal/imap/server.go:2157`.
- `needsBody` decision — `internal/imap/server.go:2176`.
- Per-message `Fetch` + `MatchMessage` — `internal/imap/server.go:2190`.
- Recursion into `NOT`/`OR` — `searchNeedsBodyRecurse` `internal/imap/server.go:3279`.

FTS replaces this scan with an index lookup that returns candidate UIDs, which
are then intersected with the remaining (flag / date / seq) criteria. When FTS
is off or fails, the current scan remains as the fallback — nothing regresses.

There is **no** existing FTS code in the tree; this is a greenfield build-out.

---

## 3. Architecture

```
  APPEND / LMTP deliver / EXPUNGE
        |  (emitMailboxChange events, internal/imap/server.go:549)
        v
  +-------------------+     index/optimize/prepend requests (TAB-delimited)
  | session pods      | ------------------------------------------------+
  | (imap / lmtp)     |                                                 |
  +-------------------+                                                 v
        |                                            +--------------------------+
        | SEARCH BODY/TEXT                           |   yarilo-fts-indexer     |
        |  -> Engine.Lookup (in-process, read)       |   queue + worker         |
        v                                            |   (async index build)    |
  +-------------------+                              +--------------------------+
  |  pkg/fts.Engine   |  <----- both read (session) and write (worker) --------+
  |  Bleve | Xapian   |
  +-------------------+
        |
        v
  per-mailbox FTS index dir  (under the mailbox index path — configurable)
```

Two independent concerns:

1. **Read path** — in the IMAP session: `Engine.Open(mailbox).Lookup(query)`.
   Cheap, no worker involved. Also computes the first-missing-UID to decide
   whether to trigger on-demand indexing.
2. **Write path** — the `yarilo-fts-indexer` worker owns index mutation
   (build + expunge + optimize). The session never blocks on a heavy index
   build; it enqueues a request and (for on-demand search) waits with a
   timeout, exactly like Dovecot's `fts-indexer.c`.

Engines are interchangeable behind one interface (§4), the way Dovecot swaps
`flatcurve` / `solr` behind `struct fts_backend_vfuncs`.

---

## 4. Engine interface (`pkg/fts`)

Isomorphic to Dovecot's `struct fts_backend_vfuncs`
(`dovecot-2.4/src/plugins/fts/fts-api-private.h:12-56`). Sketch (final signatures
settled in implementation):

```go
// Engine is a registered FTS driver (bleve, flatcurve, ...).
type Engine interface {
    Name() string
    Open(box MailboxRef) (Index, error) // one index handle per mailbox
}

// Index is the per-mailbox handle. Writes go through the worker; reads through
// the session. Concurrency is serialised by pkg/locks withMailboxLock on every
// write (project rule) plus the engine's own on-disk lock.
type Index interface {
    LastUID() (uint32, error)                 // highest indexed UID
    IsIndexed(uid uint32) (bool, error)

    BeginUpdate() (Update, error)             // indexing session
    Expunge(uid uint32) error
    Refresh() error                           // see fresh writes before a lookup
    Rescan() error                            // reconcile index vs mailbox
    Optimize() error                          // heavy compaction/merge

    Lookup(q Query, andArgs bool) (Result, error)
    Close() error
}

type Update interface {
    SetBuildKey(k BuildKey) (accept bool, err error) // false = skip this key
    BuildMore(utf8 []byte) error                     // valid UTF-8, not NUL-term
    Commit() error
}
```

### Build-key model

Copied from Dovecot (`fts-api.h:29-45`), which cleanly separates header fields
from body and attachments and gives fielded search for free:

```go
type BuildKeyType int
const (
    KeyHeader      BuildKeyType = iota // message header (hdr_name: from/to/cc/subject/...)
    KeyMIMEHeader                      // a MIME part header
    KeyBodyPart                        // text body part (ContentType, e.g. text/plain)
    KeyBodyPartBinary                  // binary part (only if engine accepts it)
)
type BuildKey struct {
    UID         uint32
    Type        BuildKeyType
    HdrName     string // for KeyHeader / KeyMIMEHeader
    ContentType string // for KeyBodyPart
}
```

### Query and result

```go
type Query struct { Args []Arg } // Arg = {Kind: BODY|TEXT|HEADER, Field, Term, Not}
type Result struct {
    DefiniteUIDs []uint32          // exact matches
    MaybeUIDs    []uint32          // candidates needing verification (filter-only engines)
    Scores       map[uint32]float64
}
```

`MaybeUIDs` mirrors Dovecot's `fts_result.maybe_uids` (`fts-api.h:67-80`): a
match found only in the combined-headers pool (not the specific field) is
"maybe" and is re-verified by the caller.

Engine flags (analogue of `enum fts_backend_flags`,
`fts-api-private.h:58-73`): `NormalizeInput`, `BuildFullWords`, `Tokenized`,
`FuzzySearch`, `BinaryMIMEParts` — let the core adapt normalization/tokenization
per engine (flatcurve is fully tokenized: `fts-backend-flatcurve.c:752`).

---

## 5. Text extraction (`internal/fts/buildmail`)

Walks the MIME structure (`github.com/emersion/go-message`, already a
dependency) and emits build-keys, mirroring Dovecot's `fts-build-mail.c`:

- Header fields → `KeyHeader` per field. Which headers are indexed is governed
  by `fts_header_includes` / `fts_header_excludes` (From/To/Cc/Subject indexed
  by default).
- Text parts → `KeyBodyPart`; `text/html` is converted to text before indexing.
- Base64 runs (≥ 50 base64-alphabet chars) are skipped so the index is not
  polluted (as Dovecot does).
- Attachments (PDF/DOC/…) → decoded to text via an optional decoder hook
  (`script` socket or Apache Tika HTTP), **Phase 3** — not in the first cut.

The Sieve `body` / `extracttext` / `foreverypart` extensions already extract
per-part text (`internal/sieve`, driven from the go-sieve fork); that logic is
the reuse/reference for the MIME walk.

---

## 6. Storage layout

Per-mailbox index, following flatcurve's model
(`fts-backend-flatcurve-xapian.cc:43-73`): one directory per mailbox, under the
mailbox's **index path**, in an `fts` subdirectory. yarilo already resolves the
index root as `UserInfo.IndexDir` (the `INDEX=` override) → `MailPath` →
`Home`, then `FolderSubpath(...)` per folder
(`internal/storage/index/file/file.go:365-380`). The FTS index lives at:

```
<index-root>/<folder-subpath>/yarilo.index.fts/
```

This is **configurable and filesystem-agnostic** — exactly like Dovecot, where
the location follows `MAILBOX_LIST_PATH_TYPE_INDEX`. An operator who wants the
FTS index on local disk points the index path there via existing config; FTS
adds no new storage-placement policy of its own. An optional
`fts_index_dir` override may be added later if a use case appears.

Key model (from flatcurve):

- **docid == UID**, 1:1, no translation table.
- Fields: `body` (unprefixed, the bulk), `all-headers` (pooled), `hdr:<name>`
  for indexed headers, plus a boolean existence term per header for
  `HEADER X` existence queries. (Xapian prefixes `A`/`H`/`B`,
  `fts-backend-flatcurve-xapian.cc:87-95`; Bleve uses named fields.)
- Commit batching (default 500 docs); shard rotation by doc count/time;
  `Optimize` merges shards. Bleve's scorch segment format merges in the
  background, so its `Optimize` is lighter than flatcurve's manual Xapian
  compaction (`fts-backend-flatcurve.c:511-530`).

Every index **write** goes through `pkg/locks` `withMailboxLock` (project rule),
in addition to the engine's own on-disk lock (flatcurve uses `flatcurve-lock`,
5 s timeout, `fts-backend-flatcurve.c:103-110`).

---

## 7. Engine #1 — Bleve (default, pure-Go)

Implements the flatcurve **design** (not its on-disk format) in pure Go:

- Per-mailbox Bleve index at the path in §6; docid == UID.
- Fields `body`, `all-headers`, `hdr:<name>`, boolean existence.
- Analyzer with Snowball stemming as the analogue of Dovecot's `lib-language`
  chain (lowercase + stemmer + stopwords).
- BM25 scoring (Bleve default) → `Result.Scores`.
- Keeps the pure-Go / static Alpine build (`CGO_ENABLED=0`, `-s -w -trimpath`).

Trade-offs to validate during implementation: language/stemming coverage vs
Dovecot's ICU+Snowball; large-mailbox performance; index size with/without
substring search.

---

## 8. Engine #2 — Xapian / flatcurve (opt-in, cgo)

An `extern "C"` shim over `libxapian` bound via cgo, reproducing flatcurve's
on-disk format byte-for-byte (docid == UID; prefixes `A`/`H`/`B`; shards
`current.###` / `index.###`; `flatcurve-lock`; rotate/optimize semantics). This
enables reading/migrating existing Dovecot flatcurve indexes.

**Build implication (called out explicitly):** Xapian is C++, so this backend
requires `CGO_ENABLED=1` and links `libxapian` + `libicu` — it **breaks** the
pure-Go static Alpine image. It therefore ships as a **separate image variant**,
not the default. This is consistent with config-not-binary (the backend is
selected by config) with one caveat: the flatcurve backend is only present in
the cgo image. The default image ships Bleve only.

Settings mirror flatcurve exactly (`fts-flatcurve-settings.c`):

| Key | Default |
|:--|:--|
| `fts_flatcurve_commit_limit` | 500 |
| `fts_flatcurve_min_term_size` | 2 |
| `fts_flatcurve_optimize_limit` | 10 |
| `fts_flatcurve_rotate_count` | 5000 |
| `fts_flatcurve_rotate_time` | 5000 (ms) |
| `fts_flatcurve_substring_search` | no |

(Note: the max term size is a hard constant of 200 bytes, not a setting; the web
docs' `rotate_size` is `rotate_count` in the 2.4 source.)

---

## 9. Indexer-worker (`yarilo-fts-indexer`)

A dedicated binary, mirroring Dovecot's `indexer` (queue) + `indexer-worker`
(`src/indexer`):

- **Wire protocol** — TAB-delimited, LF-terminated, version handshake, in line
  with yarilo's other internal protocols (see INTERNALS.md). Requests:
  `INDEX <user> <mailbox> <max-uid>`, `PREPEND …` (priority insert for on-demand
  search), `OPTIMIZE <user> <mailbox>`. Dovecot's client protocol is the model
  (`src/indexer/indexer-client.c`).
- **Worker loop** — for an `INDEX` request, walk `lastIndexedUID+1 .. uidnext-1`,
  read each message, run `buildmail` → `Engine.BeginUpdate` build-key stream,
  commit in batches (`master-connection.c:index_mailbox_precache`).
- **Triggers**:
  1. **autoindex** on save / APPEND / LMTP delivery — hook the existing
     `emitMailboxChange(EventDelivered)` events (`internal/imap/server.go:549`);
     gated by `fts_autoindex` (default off, as Dovecot).
  2. **on-demand at SEARCH** — when a body search touches unindexed mail, the
     session computes the first-missing UID, sends `PREPEND`, and waits up to
     `fts_search_timeout` (Dovecot `fts-indexer.c`, 250 ms poll).
  3. **manual** — `yarilo-admin fts rescan|optimize`.
- Deployment is config-driven (a separate Deployment/Service in Helm, like the
  other yarilo services); topology never changes the binary.

---

## 10. SEARCH integration

In `session.Search` (`internal/imap/server.go:2157`):

1. If FTS is enabled and the criteria contain `Body` / `Text` / indexed
   `Header`, call `Engine.Open(folder).Lookup(query)`.
2. Intersect `DefiniteUIDs` with the remaining non-FTS criteria (flags, dates,
   seq/UID, size, modseq) evaluated the cheap way.
3. Re-verify `MaybeUIDs` by fetching only those messages (bounded work, not the
   whole folder).
4. If the folder has unindexed mail and `fts_search_add_missing` allows,
   trigger on-demand indexing and wait; on timeout, fall back per
   `fts_search_read_fallback`.
5. If FTS is off or errors and `fts_search_read_fallback = yes` (default), run
   the existing sequential scan — behaviour is unchanged from today.

`OR` / `NOT` composition follows Dovecot: sub-args the engine can't resolve are
evaluated by the core; the engine sets a per-arg "handled" flag so the core does
not needlessly re-scan them (`fts-search.c`).

---

## 11. Configuration (`fts:` section) + Helm

Dovecot-compatible names, exposed in `pkg/config` and `helm/values.yaml`:

```yaml
fts:
  fts: bleve                       # driver: bleve | flatcurve | "" (off)
  fts_autoindex: false
  fts_autoindex_max_recent_msgs: 0
  fts_search_add_missing: body-search-only
  fts_search_read_fallback: true
  fts_search_timeout: 30s
  fts_header_includes: []          # e.g. ["From","To","Cc","Subject"]
  fts_header_excludes: []
  # attachments (Phase 3):
  fts_decoder_driver: ""           # "" | script | tika
  fts_decoder_script_socket_path: ""
  fts_decoder_tika_url: ""
  # flatcurve engine (cgo image only):
  fts_flatcurve_commit_limit: 500
  fts_flatcurve_min_term_size: 2
  fts_flatcurve_optimize_limit: 10
  fts_flatcurve_rotate_count: 5000
  fts_flatcurve_rotate_time: 5000
  fts_flatcurve_substring_search: false
```

A `components.ftsIndexer` block enables/sizes the `yarilo-fts-indexer`
Deployment. The `appVersion` bump ships in the same PR as each feature slice.

---

## 12. Phases

1. **FTS-1 (first code iteration — Bleve + worker, per the chosen scope):**
   - `pkg/fts` engine interface + build-key model.
   - `internal/fts/buildmail` (text + HTML; no attachment decode yet).
   - Bleve engine (§7).
   - `yarilo-fts-indexer` worker + wire protocol (§9): autoindex + on-demand +
     manual rescan/optimize.
   - `session.Search` integration with fallback (§10).
   - Config + Helm + `yarilo-admin fts` commands.
   - Unit + integration + live-sandbox tests (§13).
2. **FTS-2:** attachment decoders (`script` / Tika); language/stemming tuning.
3. **FTS-3 (opt-in):** Xapian / flatcurve cgo engine + separate image variant.

---

## 13. Testing

- **Unit:** `buildmail` (MIME → build-keys, header include/exclude, HTML→text,
  base64 skip); engine `Lookup` (definite vs maybe, AND/OR/NOT); docid==UID,
  expunge, rescan reconciliation; commit batching / rotation.
- **Integration (`internal/imap`):** APPEND then `SEARCH BODY` finds the
  message; EXPUNGE then search does not; header-field search hits the right
  field; FTS-off path still returns correct results via the scan fallback;
  on-demand indexing catch-up.
- **Worker:** enqueue/drain, PREPEND priority, precache walk over
  `lastIndexedUID..uidnext`.
- **Live sandbox:** deploy `yarilo-fts-indexer`, run imaptest + manual
  `SEARCH BODY/TEXT`, verify index files under the mailbox index dir, verify
  `yarilo-admin fts rescan/optimize`.

---

## 14. References

Dovecot 2.4 source (local, primary):

- FTS framework: `src/plugins/fts/fts-api.h`, `fts-api-private.h`,
  `fts-build-mail.c`, `fts-search.c`, `fts-indexer.c`, `fts-storage.c`,
  `fts-settings.c`.
- flatcurve: `src/plugins/fts-flatcurve/fts-backend-flatcurve.c`,
  `fts-backend-flatcurve-xapian.cc`, `fts-flatcurve-settings.c`.
- indexer: `src/indexer/indexer.c`, `indexer-worker.c`, `indexer-client.c`,
  `master-connection.c`.
- language layer: `src/lib-language/*`.

yarilo anchors: `internal/imap/server.go:2157` (Search), `:2176` (needsBody),
`:2190` (scan), `:549` (emitMailboxChange); `pkg/mailbox/interfaces.go:137`
(Fetch); `internal/storage/index/file/file.go:365-380` (index dir);
`pkg/config/config.go`; `internal/backend/backend.go` (wiring);
`pkg/mailbox/path.go:21` (UserInfo).

Supplementary: <https://doc.dovecot.org/main/core/plugins/fts.html>,
<https://doc.dovecot.org/main/core/plugins/fts_flatcurve.html>,
<https://github.com/slusarz/dovecot-fts-flatcurve>, <https://xapian.org/docs/>.
