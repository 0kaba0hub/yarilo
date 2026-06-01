# Phase DOVECOT-STORAGE-COMPLIANCE — design doc

**Status:** RFC, no code yet. Reviewer-eyes-only design until the
phasing + migration plan is agreed.

**Primary motivation: Dovecot → yarilo drop-in compatibility.**

The goal is that an operator running Dovecot today can point
yarilo at the existing Dovecot mailbox tree (same NFS mount,
same paths, same files, every byte of every index and every
m.\<N\> file untouched), restart sessions onto yarilo, and have
clients **see no change** — same UIDs, same UIDVALIDITY, same
flags, same modseq, same folder GUIDs. No migrator runs. No
data is rewritten. yarilo reads Dovecot's wire format as if it
were Dovecot.

This is not a "nice to have". This is what unblocks:

- **Production migrations from Dovecot installations to yarilo**
  without 12-hour offline windows for format conversion.
- **Mixed deployments** during a rolling cutover where some
  pods still run Dovecot and others run yarilo against the same
  shared storage.
- **`doveadm`-style audit tools** that operators already use,
  pointed at a yarilo-served mailbox tree.
- **Disaster recovery** where a yarilo backup is restored into
  a Dovecot installation, or vice versa.

Everything below — wire-format compliance, index byte layout,
extension registration, transaction log framing, file naming —
serves that one outcome. "Approximately Dovecot" is not
acceptable; the byte-for-byte fidelity bar is set by what
`doveadm` will accept as input.

**Project rule context:**

> **Dovecot feature parity is the North Star.** Every feature that
> exists in Dovecot is in-scope for yarilo over time.
>
> Wire formats for all internal protocols are documented in
> `INTERNALS.md`. Always consult INTERNALS.md before implementing
> any binary format or internal socket. Magic bytes, version
> numbers, field offsets — must match exactly.
>
> — `CLAUDE.md`

This doc audits the current yarilo storage layer against the
canonical Dovecot 2.4 source (`/Users/ihorru/Documents/GIT/igorru_dns/dovecot-2.4/`),
shows where yarilo silently diverged from both Dovecot and our own
INTERNALS.md, and proposes a phased rewrite to close the gap
without losing existing on-disk data.

Three drivers are in scope: **fileindex** (per-folder
`dovecot.index` analog used by every mailbox driver), **dbox**
(single-message dbox storage), **mdbox** (multi-message dbox
storage). Maildir is mostly compliant and is excluded from this
phase.

---

## 0. TL;DR

| Driver | Divergence size | Wire-compatible with Dovecot today? |
|:---|:---|:---|
| **fileindex** (`internal/storage/index/file`) | Substantial — wrong field offsets in base header, modseq+keywords baked in without `EXT_INTRO`, hand-rolled tx-log format | **No** |
| **dbox** (`internal/storage/mailbox/dbox`) | Substantial — filename is a process counter not a UID, no ASCII file-header line, no LF→CRLF body conversion, no two-phase save, hidden-dot folder layout | **No** |
| **mdbox** (`internal/storage/mailbox/mdbox`) | Fundamental — per-folder text TSV `dbox.map` instead of global binary `dovecot.map.index`; no refcount; COPY duplicates bytes; no purge; no rebuild | **No (different design)** |
| **maildir** (`internal/storage/mailbox/maildir`) | Close — `yarilo-uidlist` v3 is yarilo-specific but file layout matches | Largely yes (uidlist aside) |

Net: every Dovecot-derived storage driver in yarilo is currently
incompatible with stock Dovecot on disk. A backup taken from a
yarilo backend cannot be read by `doveadm` or any other
Dovecot-format-aware tool. This blocks: Dovecot interop testing,
mixed-deployment migrations (Dovecot ↔ yarilo), `doveadm`-style
audit tools authored against the documented format.

The rewrite is sequenced as **six PR-sized phases** below. Each
phase is independently shippable and produces a working system at
its boundary.

---

## 1. Source of truth

All numbers, struct layouts, function names, and constants in
this doc come from a single read of:

- `dovecot-2.4/src/lib-index/mail-index.h`, `mail-index-private.h`,
  `mail-transaction-log.h`, `mail-index-write.c`,
  `mail-index-sync.c`, `mail-index-lock.c`
- `dovecot-2.4/src/lib-storage/index/dbox-common/dbox-file.{c,h}`,
  `dbox-save.c`, `dbox-storage.h`
- `dovecot-2.4/src/lib-storage/index/dbox-single/sdbox-*.{c,h}`
  (storage, file, save, sync, copy, sync-rebuild, settings)
- `dovecot-2.4/src/lib-storage/index/dbox-multi/mdbox-*.{c,h}`
  (storage, file, map, map-private, save, sync, purge,
  storage-rebuild, mail)
- `yarilo/internal/storage/index/file/{file,rebuild}.go`
- `yarilo/internal/storage/mailbox/dbox/dbox.go`
- `yarilo/internal/storage/mailbox/mdbox/mdbox.go`
- `yarilo/INTERNALS.md` §7 (FileIndex) + §8 (dbox/mdbox)

When INTERNALS.md and the Dovecot source disagreed, Dovecot
source won. INTERNALS.md itself needs revision in places
(annotated below).

---

## 2. fileindex (per-folder dovecot.index) — divergences

yarilo's `internal/storage/index/file` implements a per-folder
binary index inspired by Dovecot's `mail-index`. It is used by
ALL three mailbox drivers (maildir, dbox, mdbox) as the per-folder
state store. It is NOT wire-compatible with Dovecot's
`mail-index`.

### 2.1. Base header — field collisions

Dovecot `struct mail_index_header` (120 bytes,
`mail-index.h:89-177`) at major=7 minor=3 has these byte offsets:

| Offset | Size | Field |
|:---|:---|:---|
| 0  | 1  | `major_version` |
| 1  | 1  | `minor_version` |
| 2  | 2  | `base_header_size` |
| 4  | 4  | `header_size` |
| 8  | 4  | `record_size` |
| 12 | 1  | `compat_flags` |
| 13 | 3  | padding |
| 16 | 4  | `indexid` |
| 20 | 4  | `flags` (CORRUPTED, HAVE_DIRTY, FSCKD) |
| 24 | 4  | `uid_validity` |
| 28 | 4  | `next_uid` |
| 32 | 4  | `messages_count` |
| 36 | 4  | `unused_old_recent_messages_count` |
| 40 | 4  | `seen_messages_count` |
| 44 | 4  | `deleted_messages_count` |
| 48 | 4  | `first_recent_uid` |
| 52 | 4  | `first_unseen_uid_lowwater` |
| 56 | 4  | `first_deleted_uid_lowwater` |
| 60 | 4  | `log_file_seq` |
| 64 | 4  | `log_file_tail_offset` |
| 68 | 4  | `log_file_head_offset` |
| 72 | 4  | `unused_old_sync_size_part1` |
| 76 | 4  | `log2_rotate_time` |
| 80 | 4  | `last_temp_file_scan` |
| 84 | 4  | `day_stamp` |
| 88 | 32 | `day_first_uid[8]` |

yarilo's `writeHeader` (`file.go:723-754`) writes at major=7 minor=4
with these offsets:

| yarilo offset | yarilo field | Dovecot field at same offset | Collision? |
|:---|:---|:---|:---|
| 16 | `indexID` | `indexid` | OK |
| 24 | `uidValidity` | `uid_validity` | OK |
| 28 | `nextUID` | `next_uid` | OK |
| 32 | `msgCount` | `messages_count` | OK |
| 36 | `seenCount` | `unused_old_recent_messages_count` | **WRONG** |
| 40 | `deletedCount` | `seen_messages_count` | **WRONG** |
| 44 | `logFileSeq` | `deleted_messages_count` | **WRONG** |
| 48 | `logFileTail` | `first_recent_uid` | **WRONG** |
| 52 | `logFileHead` | `first_unseen_uid_lowwater` | **WRONG** |
| 56 | `modseq (8 bytes)` | `first_deleted_uid_lowwater` + `log_file_seq` | **WRONG** (8-byte field straddles two Dovecot fields) |
| 64 | `guid (16 bytes)` | `log_file_tail_offset` + `log_file_head_offset` + `unused_old_sync_size_part1` + `log2_rotate_time` | **WRONG** (16-byte field consumes 4 Dovecot fields) |

yarilo's `flags` slot at offset 20 is hard-zeroed — never sets
`FSCKD` even when rebuild detects corruption.

Result: a Dovecot reader pointed at a yarilo `.index` would see
the right `next_uid` and `uid_validity` but wrong `messages_count`
(reads `seen_messages_count` instead), wrong `log_file_seq`
(reads `deleted_messages_count`), wrong log offsets, and
zero-bytes for the unused-but-defined Dovecot fields.

### 2.2. Records — extensions without EXT_INTRO

Dovecot:
- Base record = 5 bytes (`uid` + `flags`).
- Extra per-record bytes (modseq, keywords, ...) MUST be
  introduced via a `MAIL_TRANSACTION_EXT_INTRO` tx record (and
  recorded as `mail_index_ext_header` entries in the extended
  header section between `base_header_size` and `header_size`).
- Readers derive each extension's per-record offset from the
  EXT_INTRO records they replayed.

yarilo:
- `recordSize = 5 + 8 + 4 = 17` (base + modseq + keyword bitmask),
  hard-coded.
- No EXT_INTRO records written.
- `header_size = base_header_size` — no extended-header section
  at all (`file.go:730`).
- A Dovecot reader would see `record_size=17` but zero
  registered extensions, treat the trailing 12 bytes as opaque.

### 2.3. Transaction log (.index.log) format

Dovecot `mail_transaction_log_header` (`mail-transaction-log.h:36-84`)
is 28 bytes (v1.3):

```
uint8  major_version    // 1
uint8  minor_version    // 3
uint16 hdr_size         // ≥24 (MAIL_TRANSACTION_LOG_HEADER_MIN_SIZE)
uint32 indexid          // must match index header
uint32 file_seq         // bump on rotation
uint32 prev_file_seq    // links to .log.2
uint32 prev_file_offset
uint32 create_stamp
uint64 initial_modseq   // since v1.1
uint8  compat_flags     // since v1.2
uint8  unused[3]
uint32 unused2
```

yarilo's `writeLogHeader` (`file.go:1020-1032`) writes 32 bytes
with different field positions: `initial_modseq` at offset 18
instead of 26, `create_stamp` at offset 26 instead of 22, no
`prev_file_seq/prev_file_offset` slots. Wire-incompatible.

Per-record framing (Dovecot, `mail-transaction-log.h:189-196`):

```
struct mail_transaction_header {
    uint32 size;   // record + header size; encoded via mail_index_uint32_to_offset()
    uint32 type;   // bitmask: type | EXTERNAL_FLAG | SYNC_FLAG
}
// payload depends on type
```

Critical: Dovecot's `size` is mangled through
`mail_index_uint32_to_offset()` so a torn-write of half the field
cannot look like a valid size. yarilo writes `size` as a plain
little-endian uint32 — no mangling.

yarilo's `appendLogRecord` (`file.go:1034-1050`) collapses every
tx type into a single 25-byte row of
`{size, type, uid, flags, modseq, keywordBits}`. Dovecot has a
distinct payload struct per type (see §3 in mail-index agent
report — APPEND uses `mail_index_record[]`, EXPUNGE uses
`mail_transaction_expunge`, FLAG_UPDATE uses
`mail_transaction_flag_update`, etc.). They share **no bytes**
with yarilo's row.

Tx types covered:

| Dovecot type | Dovecot const | yarilo support |
|:---|:---|:---|
| APPEND | `0x02` | Custom shape; data ≠ Dovecot |
| EXPUNGE | `0x01` | Custom shape; no EXPUNGE_PROT |
| EXPUNGE_GUID | `0x2000` | Not implemented |
| FLAG_UPDATE | `0x04` | Custom shape |
| HEADER_UPDATE | `0x20` | Not implemented |
| EXT_INTRO | `0x40` | Custom shape; not Dovecot-readable |
| EXT_RESET | `0x80` | Not implemented |
| EXT_HDR_UPDATE | `0x100` | Not implemented |
| EXT_HDR_UPDATE32 | `0x10000` | Not implemented |
| EXT_REC_UPDATE | `0x200` | Not implemented |
| EXT_ATOMIC_INC | `0x1000` | Not implemented (required for mdbox refcount) |
| KEYWORD_UPDATE | `0x400` | Custom shape |
| KEYWORD_RESET | `0x800` | Not implemented |
| MODSEQ_UPDATE | `0x8000` | Not implemented |
| INDEX_DELETED / UNDELETED | `0x20000 / 0x40000` | Not implemented |
| BOUNDARY | `0x80000` | Not implemented |
| ATTRIBUTE_UPDATE | `0x100000` | Not implemented |

### 2.4. `.names` sidecar — yarilo invention

yarilo writes a `<folder>/.index.names` text file mapping UID →
filename (`file.go:1189-1218`). Dovecot has no equivalent — it
stores filenames either implicitly (Maildir filename is the
identity), via `dovecot-uidlist` v3 (Maildir), via inline cache
fields (sdbox), or doesn't need them (mdbox uses map_uid).

### 2.5. Other Dovecot features missing entirely

- `dovecot.index.cache` (per-folder cache file holding cached
  headers, vsize, etc.) — yarilo has no cache layer.
- Index `flags` field with `MAIL_INDEX_HDR_FLAG_FSCKD` —
  yarilo never sets corruption flags.
- `mail_index_uint32_to_offset()` mangling — protection against
  torn writes.
- `KEEP_BACKUPS` mode that hard-links `.index` to `.index.backup`
  before rewrite (`mail-index-write.c:13-55`).

---

## 3. dbox (sdbox single-message) — divergences

yarilo's dbox is a hand-rolled approximation that shares the
file's metadata key-line convention with Dovecot but misses
nearly everything else.

### 3.1. Filename rule

| Aspect | Dovecot sdbox | yarilo dbox |
|:---|:---|:---|
| Format constant | `SDBOX_MAIL_FILE_FORMAT "u.%u"` (`sdbox-storage.h:9`) | `"u.%016x"` |
| Encoding | Decimal, no padding | 16 lowercase hex, zero-padded |
| Source value | **IMAP UID** from per-folder `mail_index_header.next_uid` | Per-process atomic counter (`b.counter.Add(1)`) |

Result: yarilo's `u.0000000000000001` is the **first save by this
yarilo-imap process**, not the message with IMAP UID=1. After
process restart the counter resets to 0 — meaningful collisions
possible without the parallel `UserIndex` mapping.

### 3.2. dbox file format

Dovecot two-layer format (`dbox-file.{c,h}`):

```
<file header line, ASCII>           e.g. "2 M20 C68d9c4a1\n"
<dbox_message_header, 32 bytes>     magic + type + UID slot + size
<body, message_size bytes>          LF→CRLF converted
<dbox_metadata_header>              "\n\001\003\n"
<key><value>\n  ...                 G, R, Z?, V, P, O, B, X
<empty line "\n">                   metadata terminator
```

yarilo skips the ASCII header line entirely and writes a 31-byte
message header (one byte short of Dovecot's 32 — drops the
trailing `\n`). The yarilo header pattern is
`"\x01\x02N %08x %016x\n"` which DOES use the same 8-hex UID slot
Dovecot has, but writes `00000000` literally instead of leaving 8
spaces as Dovecot v2 does. INTERNALS.md §8 line 562 documents
this incorrectly — it claims the UID is in the header, but
Dovecot v2 specifically leaves that field unused (see
`dbox_msg_header_fill` at `dbox-file.c:764-774`).

### 3.3. Body encoding — LF→CRLF

Dovecot pipes the input message through `istream-crlf` before
write (`dbox-save.c:46`). Body bytes on disk are CRLF-terminated.
`message_size_hex` in the header reflects the post-conversion
size; `V` metadata is the same.

yarilo writes raw bytes (`dbox-save.go:Save`), no conversion.
Body bytes match the input verbatim.

Consequence: yarilo's IMAP `RFC822.SIZE` and `FETCH BODY[]` will
return the LF-counted size for messages saved natively, but
CRLF-counted size for messages received over IMAP APPEND (which
the IMAP protocol delivers in CRLF form). Mixed sizes for the
same logical content — RFC 3501 violation under specific paths.

### 3.4. Two-phase save

Dovecot:
1. Write to `.temp.<sec>.P<pid>Q<seq>M<usec>.<host>` in the same
   directory.
2. Stream body, write metadata, pwrite real header.
3. Commit the per-folder index transaction (allocates UIDs).
4. `rename(2)` each temp file to `u.<UID>` (`sdbox_file_assign_uid`,
   `sdbox-file.c:149-184`).

yarilo writes the final filename directly via `O_CREAT|O_EXCL`. A
crash mid-write leaves a half-written `u.<seq>` file visible. No
recovery of orphan temp files (`DBOX_TMP_DELETE_SECS = 36h`
cleanup not implemented).

### 3.5. Folder layout

| Aspect | Dovecot sdbox | yarilo dbox |
|:---|:---|:---|
| Mailbox parent | `<home>/sdbox/mailboxes/` | `<home>/` directly |
| Per-folder dir | `<...>/<folder>/dbox-Mails/` | `<...>/INBOX/` or `<...>/.<folder>/` (Maildir-style hidden) |
| Per-user uidvalidity | `<home>/sdbox/control/dovecot-uidvalidity` | None |
| Index colocation | `dovecot.index*` siblings of `u.*` | yarilo's fileindex separate, different file naming |
| Mailbox GUID storage | `dbox-hdr` extension in folder index | yarilo's folder GUID is in fileindex header at the wrong offset (see §2.1) |

A doveadm pointed at a yarilo dbox tree would not find any
mailboxes — wrong directory layout.

### 3.6. Other operations

- **COPY**: Dovecot tries `link(2)` hardlink (`sdbox_copy_hardlink`,
  `sdbox-copy.c:92`), falls back to stream-copy. yarilo's
  `MailboxBackend` has no COPY primitive at all — IMAP COPY
  reads the full message and Save's it back.
- **Rebuild**: Dovecot walks the dir, parses each filename as a
  UID, appends to mail_index. yarilo's Phase BACKEND-API-EASY
  rebuild (already merged in v1.25) walks, parses the trailer,
  builds new MessageMeta — but since yarilo's filenames don't
  carry UIDs, UIDs are reassigned, breaking client UID caches.
- **Alt storage** (`dbox-alt-root`) — completely absent in yarilo.

---

## 4. mdbox (multi-message dbox) — divergences

This is the most severe gap. yarilo's mdbox is not a buggy
Dovecot mdbox — it's a **fundamentally different design** that
happens to share file naming.

### 4.1. Storage model — global vs per-folder

| Aspect | Dovecot mdbox | yarilo mdbox |
|:---|:---|:---|
| Per-user storage path | `<home>/mdbox/storage/` | `<home>/mdbox-storage/` |
| Map index | Global binary `dovecot.map.index` (a real mail-index) | Per-folder text TSV `<folder>/dbox.map` |
| Map record | `{file_id, offset, size}` + `refcount` (uint16) + auto-allocated `map_uid` | TSV row `<file_id> <offset> <size> <expunged>` |
| Per-folder index extension | `mdbox`: `{map_uid, save_date}` (8 bytes per message) | None — folder maps directly to physical |
| `map_uid` allocation | Monotonic global counter inside `dovecot.map.index` (allocated under map atomic lock) | Doesn't exist |
| `refcount` | uint16 per map record (separate "ref" extension); incr on COPY, decr on EXPUNGE | Doesn't exist |

The map is the keystone of mdbox: it's what makes COPY O(1) and
what survives folder-index corruption.

### 4.2. Save flow

Dovecot `mdbox-save.c`:
1. `mdbox_map_append_next` — find an appendable m.* (reuse last
   one if not over `mdbox_rotate_size`, default 2 MiB; create
   new if none).
2. Stream body, write metadata.
3. `mdbox_map_atomic_lock` — take the map sync lock.
4. `mdbox_sync_begin` — take the folder sync lock (strict order:
   map first, folder second).
5. `mdbox_map_append_assign_map_uids` — assign new file_ids to
   any new m.* files (under map lock from `highest_file_id+1`),
   append map records with `(file_id, offset, size, refcount=1)`,
   allocate map_uids via `mail_index_append_finish_uids`.
6. `mdbox_save_set_map_uids` — write `{map_uid, save_date}` into
   the per-folder index for each new message.
7. Allocate per-folder UID from `folder_index.next_uid`.
8. Commit both transactions.

yarilo `mdbox.Save`:
1. Re-stat current m.<id> file size under per-folder lock.
2. If >threshold: `currentFileID++` (per-handle!).
3. Append message to m.<id>.
4. Append a row to per-folder dbox.map.
5. Return `<file_id>:<offset>` as opaque "filename" token.

The cross-process `file_id` race the current PR addressed via
COUNTER-INC doesn't even apply to Dovecot — file_id is assigned
under the map atomic lock, so no two processes ever pick the
same id.

### 4.3. COPY / MOVE — the O(1) magic

Dovecot `mdbox_copy` (`mdbox-save.c:438-496`):
1. Look up source's per-folder `{map_uid, save_date}` record.
2. Push `map_uid` into `copy_map_uids` array.
3. `mail_index_update_ext` writes the same `map_uid` into a new
   per-folder record in the destination folder.
4. On commit, `mdbox_map_update_refcounts(+1)` increments the
   refcount in the global map.

**Zero bytes are read or copied.** One folder-index append + one
atomic refcount inc. Same path drives MOVE (copy with +1, then
expunge with -1; refcount net change zero, but message visible
in destination).

yarilo: IMAP COPY reads the entire source message and Save's it
to the destination, allocating new file_id/offset and writing
all bytes to disk again. For a 50-message MOVE of 10 MB messages
that's 500 MB of disk I/O vs Dovecot's ~50 index appends.

### 4.4. Expunge

Dovecot (`mdbox-sync.c`):
1. Lock map index.
2. Read per-folder record's map_uid, decrement that map_uid's
   refcount via `mail_index_atomic_inc_ext`.
3. Expunge the per-folder index record.
4. Bytes stay in m.<N>.

yarilo: flips `expunged=1` in per-folder TSV. Bytes stay
forever — no refcount, no global state.

### 4.5. Purge (compaction)

Dovecot `mdbox-purge.c` (~530 lines):
- Walks the global map, finds m.<N> files containing
  refcount==0 records.
- For each such file, under per-file flock: copy live records to
  a new m.<N> appendable file, push old map_uids into
  `copied_map_uids` (with new file_id/offset) or
  `expunged_map_uids`.
- `mdbox_map_append_move` atomically rewrites map records for
  copied map_uids (same map_uid, new physical location) and
  expunges records for purged map_uids. **All folders that
  referenced these map_uids continue to work without any
  per-folder I/O** — that's why the global map matters.
- Unlink old m.<N>.

yarilo: not implemented. Disk space grows monotonically as users
expunge messages.

### 4.6. Rebuild

Dovecot `mdbox-storage-rebuild.c` (~1052 lines):
1. Walk `storage/` + `mdbox-alt/storage/`, open each m.<N>.
2. For each record in each m.<N>: parse metadata, extract GUID +
   size, push into `msgs` array, also index by GUID hash.
3. Reconcile against existing map (preserve map_uids where
   possible).
4. Append fresh map records for any orphans not in old map.
5. For each per-folder index: walk its records, prefer GUID
   lookup (uses `guid_ext_id` extension we don't have) over
   map_uid lookup, re-append with resolved `{map_uid, save_date}`.
6. Update refcounts based on rebuilt folder records.
7. Orphan messages (refcount==0 after folder pass) restored to
   `INBOX` (or `DBOX_METADATA_ORIG_MAILBOX` if present).
8. Bump `rebuild_count` in map extension header.

yarilo's Scan implementation I added on the parked branch reads
the per-folder dbox.map and skips expunged rows. It cannot
recover orphans (no GUID hash, no global map), cannot detect
corrupted m.<N> records, cannot rebuild the map (there is none).

### 4.7. Locking model

Dovecot:
- **Map atomic lock** (mail_index sync lock on dovecot.map.index)
  — user-wide. Held during map_uid allocation, file_id assignment,
  refcount changes.
- **Per-folder index sync lock** (mail_index sync lock on
  folder's dovecot.index) — acquired after map lock when expunge
  is involved. Strict map→folder order avoids deadlock.
- **Per-file flock** on m.<N> — held during purge of that file
  and during a single session's append (so the per-folder lock
  isn't needed for append correctness).

yarilo: only per-folder X-lock via `pkg/locks`. No global map
lock (there's no global map). No per-file flock. Cross-process
correctness "works" today only because each user lives on a
single backend tag, so contention is intra-pod.

---

## 5. Migration scenarios

Three production scenarios — listed in order of priority:

**Scenario C (PRIMARY) — Dovecot installation on disk, yarilo
runs on top.** Operator stops Dovecot, points yarilo at the
existing storage tree (same paths, same files, untouched).
yarilo reads every byte exactly as Dovecot wrote it. **No
migrator runs.** No data is transformed. Sessions resume with
identical UIDs, UIDVALIDITY, modseq, flags. This is the use
case driving the entire compliance phase — see motivation in
the intro.

Acceptance criteria for Scenario C:
- yarilo opens `<home>/sdbox/mailboxes/INBOX/dbox-Mails/u.<UID>`
  files written by Dovecot 2.4 and serves them on IMAP FETCH
  unchanged.
- yarilo reads `<home>/mdbox/storage/dovecot.map.index` written
  by Dovecot 2.4 — every map_uid, every refcount, every
  file_id+offset — and uses it as the authoritative state.
- yarilo writes new messages in the same format; if Dovecot is
  ever pointed back at the storage, Dovecot reads yarilo's
  writes without trouble. **Bidirectional file-level
  compatibility.**
- Wire-level: `pkg/mailindex` round-trips
  Dovecot-produced fixtures byte-for-byte.

**Scenario A (SECONDARY) — yarilo-legacy data on disk, upgrade
in place.** Any existing yarilo deployment that wrote data with
the v1.x non-Dovecot format. Data must be migrated to
Dovecot-compliant before the new code can serve it.

The `yarilo-migrate storage` tool handles Scenario A; details
in §7 (migrator subsection). Scenario A is one-way:
yarilo-legacy → Dovecot-compliant. The legacy format reader is
preserved only inside the migrator and is removed from the
runtime path after migration completes.

**Scenario B (TERTIARY) — fresh deployment.** No legacy data.
Wire format is Dovecot from day one, no migrator runs.

Decisions needed (open questions for review, §10):

1. **Are there production deployments yet with real user data?**
   If no — Scenario A migrator is best-effort, not blocking. If
   yes — migrator is on the critical path. Scenario C does NOT
   depend on this answer (it's about Dovecot data, not yarilo
   data).
2. **Can we declare a flag day for Scenario A** (operator runs
   `yarilo-migrate storage` while sessions are stopped), or
   must migration be online (sessions continue, migrator drains
   folder-by-folder)?
3. **Scenario C operational contract.** Should the v2.x release
   ship explicit documentation + a smoke test
   (`yarilo-smoketest dovecot-compat`) demonstrating that a
   Dovecot 2.4 mailbox is read correctly? Recommendation: YES,
   the smoketest is the operator's proof that drop-in works.

---

## 6. Proposed architecture

Three new packages + rework of two existing:

```
NEW: pkg/mailindex         — generic Dovecot mail-index v7 reader/writer
                              header (extended), records (extensions),
                              transaction log (every Dovecot tx type),
                              sync (lock + recreate + .log rotation),
                              fixture tests against Dovecot-produced files

NEW: internal/storage/mdbox/map   — mdbox-map.c equivalent in Go
                                     opens dovecot.map.index via pkg/mailindex,
                                     registers "map" + "ref" extensions,
                                     atomic lock + transaction context,
                                     append_next / assign_map_uids / update_refcounts /
                                     lookup / get_file_msgs / get_zero_ref_files /
                                     append_move (the purge primitive)

NEW: internal/storage/mailbox/dboxv2  — Dovecot-compliant dbox file format
                                         (header line + message_header struct +
                                         CRLF body + metadata block). Shared
                                         between sdbox and mdbox drivers.

REWORK: internal/storage/index/file  — refactor onto pkg/mailindex.
                                        Per-folder index becomes a thin
                                        client that registers "modseq",
                                        "keywords", "dbox-hdr" extensions.
                                        Folder GUID + rebuild_count move
                                        into the dbox-hdr extension where
                                        they belong (no more byte-offset
                                        collisions with Dovecot fields).

REWORK: internal/storage/mailbox/{dbox,mdbox}  — rewritten on top of
                                                  pkg/mailindex + dboxv2 +
                                                  (for mdbox) the new map
                                                  package. Old yarilo
                                                  implementations stay as
                                                  yarilolegacy package for
                                                  the migration tool.

NEW: app/yarilo-migrate/storage  — one-shot migrator that converts
                                    a user's home from yarilo-legacy
                                    to Dovecot-compliant. Reads via
                                    yarilolegacy, writes via the new
                                    drivers, verifies (UID parity,
                                    message body checksums), atomically
                                    swaps the home symlink/path.
```

The `pkg/mailindex` library is the unlock. It's the prerequisite
for every later phase. Without it, mdbox map.index has no
foundation and per-folder fileindex stays incompatible.

---

## 7. Phased rollout

Six phases. Each one a separate PR with its own commit, tests,
helm/version bump, docs.

### Phase 1 — `pkg/mailindex` (foundation)

**Goal:** Wire-compatible mail-index v7.3 implementation in Go.

**Scope:**
- Header encode/decode (all 120 bytes, every field in spec).
- Extension registration: `Register(name, hdrSize, recordSize, align) -> id`.
- Extended-header read/write (sequence of `mail_index_ext_header`
  records in `header_size - base_header_size` region).
- Records: base 5 bytes + per-ext data via offset table derived
  from registered extensions.
- Transaction log:
  - Header struct (28 bytes), `file_seq` rotation chain.
  - Per-record framing with `mail_index_uint32_to_offset()`
    mangling.
  - Encoder/decoder for **every** Dovecot tx type (table in
    §2.3). Each as its own typed Go struct with Encode/Decode.
- Sync:
  - Lock acquisition via `pkg/locks` (own resource family,
    e.g. `mailindex:<file>:lock`).
  - `Recreate(path, header, extHdr, records)` — atomic rewrite
    via `.tmp` + rename, optional `.backup` hardlink.
- Test fixtures: pre-generated Dovecot 2.4 `.index` / `.index.log`
  files for round-trip parity tests (read with `pkg/mailindex`,
  re-write, byte-compare; or read Dovecot bytes, read with
  `pkg/mailindex`, assert all fields).

**Size:** ~2000 lines + ~1000 lines tests.

**Out of scope for this phase:** any per-folder business logic.
pkg/mailindex is purely the binary format.

**Acceptance:** `pkg/mailindex` reads and round-trips a Dovecot
2.4 v7.3 `dovecot.index` + `dovecot.index.log` pair byte-for-byte.

---

### Phase 2 — fileindex rewrite onto `pkg/mailindex`

**Goal:** Make yarilo's per-folder index Dovecot-compatible
without changing its public Go API.

**Scope:**
- New `internal/storage/index/file/v2.go` implementation that
  uses `pkg/mailindex`.
- Register `modseq` (8 bytes), `keywords` (4-byte bitmask, can
  evolve to N-byte later), `dbox-hdr` (mailbox GUID + rebuild
  count) as proper Dovecot extensions.
- Replace yarilo's `.names` sidecar with extension-or-cache
  approach (decision needed — see open questions).
- Same `UserIndex` Go interface, no callers change.
- Read-old / write-new migration logic inside `OpenFolder`:
  detect yarilo-legacy `.index` (by checking for the impossible
  header layout: modseq at offset 56 means yarilo-legacy),
  read it via a tiny legacy decoder, rewrite as Dovecot-compliant
  on first open.

**Size:** ~1200 lines new + ~400 lines legacy reader + ~600
lines tests.

**Acceptance:** Existing yarilo tests pass against the new
implementation. A Dovecot-compatible `.index` written by yarilo
is readable by `pkg/mailindex` round-trip and by Dovecot's
`doveadm` if available for testing.

---

### Phase 3 — `dboxv2` shared format + sdbox rewrite ✅ shipped (v1.28.0 + v1.29.0 consumer switch)

**Goal:** Replace `internal/storage/mailbox/dbox` with a
Dovecot-compliant single-message dbox driver.

**Status:** Driver landed at `internal/storage/mailbox/dboxv2/`
with full Dovecot file format (file header line + 32-byte
`dbox_message_header` + body + metadata block with
G/R/Z/V/P/O/B/X keys). Atomic publish: `Save(folder, r, uid, …)`
writes `.temp.*` then renames to `u.<uid>` under the mailbox X
lock — no orphan temp on crash, no separate AssignUID call.
`Copy(srcFolder, srcFilename, dstFolder, dstUID)` uses `link(2)`
for O(1) IMAP COPY directly into the destination's final name.
Folder layout `<home>/sdbox/mailboxes/<folder>/dbox-Mails/` and
per-user `<home>/sdbox/control/dovecot-uidvalidity` materialised
in `Init()`. Legacy reader at
`internal/storage/mailbox/dbox/v1legacy/` for the migrator.

**v1.29.0 follow-up — interface refactor:**
- `UserMailbox.Save` accepts a UID parameter; `AppendUIDEntry`
  removed from the interface (maildir writes its `dovecot-uidlist`
  sidecar inline inside Save, sdbox renames in Save, mdbox ignores)
- `UserIndex.AllocateAppend` split into `AllocateUID(folderID)` +
  `AppendMessage(folderID, meta)` — UID is allocated **before**
  the message body is written. Crash between allocate and append
  burns the UID (matches Dovecot semantics)
- Session sequence: `uid := idx.AllocateUID(folderID); filename :=
  box.Save(folder, r, uid, size, flags); idx.AppendMessage(folderID,
  meta)`
- Old `internal/storage/mailbox/dbox` package deleted (v1legacy
  reader stays at `internal/storage/mailbox/dbox/v1legacy/` for
  the migrator)
- Backend factory accepts `sdbox` as canonical driver name; `dbox`
  remains as an alias for ops migrating from older configs

**Scope:**
- `internal/storage/mailbox/dboxv2/` containing the shared
  format helpers (file header, message_header, metadata block,
  CRLF body conversion) and the sdbox-specific Save/Fetch/Remove.
- `MailboxBackend` gains a `Copy(srcFolder, srcFilename,
  dstFolder) (string, error)` primitive so sdbox can hardlink
  instead of stream-copy.
- Two-phase save: Save returns `(tempPath, err)`; new
  `AssignUID(folder, tempPath, uid uint32)` does the rename.
  `UserIndex` drives the order (allocate UID, then assign).
- Folder layout: `<home>/sdbox/mailboxes/<folder>/dbox-Mails/`.
- Per-user `<home>/sdbox/control/dovecot-uidvalidity` counter.
- Mailbox GUID in `dbox-hdr` extension (from Phase 2).
- Legacy reader package `internal/storage/mailbox/dbox/v1legacy`
  for the migrator.

**Size:** ~900 lines new + ~300 lines tests.

**Acceptance:** A folder saved via the new driver is readable by
doveadm (if test fixture available). UIDs preserved across save +
fetch. Rebuild via existing `/api/backend/index/rebuild` produces
identical UIDs to those originally assigned (now possible because
UID is in the filename).

---

### Phase 4 — `mdboxmap` package ✅ shipped (v1.30.0)

**Goal:** Implement Dovecot's `mdbox-map.c` in Go using
`pkg/mailindex`.

**Status:** Package landed at
`internal/storage/mailbox/mdbox/mdboxmap/`. Drop-in compatible
with Dovecot 2.4 `dovecot.map.index` on-disk format:
- "map" extension — 12 B per record `(file_id, offset, size)`,
  4 B header `highest_file_id`
- "ref" extension — 2 B uint16 refcount per record
- `Record.UID` column is the canonical `map_uid`

Public surface:
- `Open(path, username, WithLocker(...))` — opens or creates
- `AppendBatch().Next(size) → (file_id, offset)` + `Finish()
  → []map_uid` — reserves a strictly-increasing UID range under
  the cross-process map X lock (`locks.MdboxMapKey(user)`),
  rolls to a new `m.<N>` when the cumulative offset would
  exceed `mdbox_rotate_size` (2 MiB default).
- `Lookup(map_uid)` / `LookupMany([]uid)` — O(1)
- `UpdateRefcounts(uids, delta)` — clamps at 0/0xFFFF; missing
  UIDs surface as errors (caller bug)
- `GetZeroRefFiles()` / `CompactGarbage()` — for purge driver
  discovery
- `RecordsInFile(file_id)` — every entry living in one m.<N>
- `AppendMove(moved, expunged)` — atomic rewrite of physical
  pointers + expunge of zero-ref records (purge primitive)

Lock plumbing follows the strict **map-then-folder** order
documented in §4.2: `MdboxMapKey(user)` first, then
`MailboxKey(user, folder)`. Per-process `sync.Mutex` is the
fast-path; cross-process serialisation routes through
`locks.Acquire` with the standard 30s TTL.

Phase 5 (mdbox driver rewrite) consumes this package; nothing in
the codebase imports it yet beyond its own tests.

**Scope:**
- `internal/storage/mailbox/mdbox/mdboxmap/` package.
- `Open(path) -> *Map` — allocate via `mailindex.Open`, register
  "map" + "ref" extensions, validate `highest_file_id` and
  `rebuild_count` header.
- `(m *Map) AtomicBegin/Lock/Finish` — wrap mail_index sync.
- `(m *Map) AppendBegin/Next/Finish` — find appendable m.* (with
  the same backward-lookup-up-to-10-files heuristic Dovecot
  uses).
- `AssignFileIDs / AssignMapUIDs` — under atomic lock.
- `UpdateRefcounts(map_uids []uint32, delta int16)` — via
  `EXT_ATOMIC_INC`.
- `Lookup(map_uid) -> {file_id, offset, size, refcount}`.
- `GetZeroRefFiles() -> []file_id`.
- `AppendMove(copied, expunged)` — the primitive used by purge.

**Size:** ~1500 lines + ~800 lines tests (incl. miniredis-style
fake mailindex for fast unit tests).

**Acceptance:** Round-trips Dovecot 2.4-produced
`dovecot.map.index` fixtures. Concurrent goroutine stress test
shows no lost map_uid allocations under simulated backend
contention.

---

### Phase 5 — mdbox driver rewrite

**Goal:** Replace `internal/storage/mailbox/mdbox` with a
Dovecot-compliant multi-message driver on top of `mdboxmap` +
`dboxv2` + `pkg/mailindex`.

**Scope:**
- New driver at `internal/storage/mailbox/mdbox/` (overwriting
  the existing one; old impl moves to `mdbox/v1legacy` for
  migrator).
- Folder layout: `<home>/mdbox/storage/m.<N>` +
  `<home>/mdbox/storage/dovecot.map.index` +
  `<home>/mdbox/mailboxes/<folder>/dovecot.index`.
- Save: map AppendNext → write body → assign map_uid → write
  per-folder `{map_uid, save_date}` record. Strict
  map-then-folder lock order. Stream body (no `io.ReadAll`).
- Copy (O(1)): read source map_uid, write same map_uid into
  destination folder, refcount +1.
- Move: Copy + source Expunge.
- Expunge: refcount -1 then expunge per-folder record.
- File rotation: under map lock, allocate file_id from
  `highest_file_id+1`.
- Per-folder index `mdbox` extension `{map_uid, save_date}` +
  `mdbox-hdr` extension `{map_uid_validity, mailbox_guid, flags}`
  + `guid` extension (16 bytes GUID per message, for
  rebuild-by-GUID resolution).

**Size:** ~1800 lines + ~1000 lines tests.

**Acceptance:** Full IMAP COPY test moves 100 messages between
folders in ≤O(folders) disk I/O, not O(messages × bytes).
Concurrent multi-process writers don't corrupt the map (stress
test with 2+ yarilo-imap pods saving to same user).

---

### Phase 6 — mdbox purge + rebuild

**Goal:** Reclaim disk after expunge; recover from map corruption.

**Scope:**
- `internal/storage/mailbox/mdbox/purge.go` — implementation of
  `mdbox-purge.c` algorithm. Backend-api gains
  `POST /api/backend/mdbox/purge` + CLI
  `yarilo-admin backend mdbox purge <user>`.
- `internal/storage/mailbox/mdbox/rebuild.go` — implementation
  of `mdbox-storage-rebuild.c`. Backend-api `index rebuild` for
  mdbox folders now succeeds (no more 501).
- Optional automatic purge trigger: per-Dovecot
  `mdbox_purge_preserve_alt` settings → Helm values; background
  worker that triggers purge when global expunged-bytes-ratio
  crosses a threshold.

**Size:** ~1500 lines + ~600 lines tests.

**Acceptance:** End-to-end test: deliver N messages, expunge
half, purge — disk usage drops by ~50%. Inject map.index
corruption, run rebuild — full state recovered.

---

### Plus: `yarilo-migrate storage` (interleaved with Phases 2/3/5)

**Goal:** One-shot online or offline migration of yarilo-legacy
data to Dovecot-compliant.

**Scope:**
- Reads via legacy decoder packages
  (`internal/storage/.../v1legacy/`).
- Writes via new drivers.
- Verification: UID parity, body sha256 checksum, flags +
  modseq parity.
- Atomic publish: write to `<home>.migrated`, swap symlink,
  delete legacy.
- CLI: `yarilo-migrate storage --user <u>` per-user; `--all`
  iterates the userdb.

**Size:** ~1500 lines + ~600 lines tests.

---

## 8. Sequencing + critical path

```
Phase 1 (pkg/mailindex) ─┬─→ Phase 2 (fileindex rewrite) ─┐
                          ├─→ Phase 3 (sdbox)              ├─→ Migrator
                          │                                │
                          └─→ Phase 4 (mdboxmap) ──→ Phase 5 (mdbox) ──→ Phase 6 (purge/rebuild)
```

Phase 1 unblocks everything. Phases 2 + 3 + 4 can run in
parallel after Phase 1 lands. Phase 5 blocks on 4. Phase 6
blocks on 5. Migrator integrates piece by piece.

Total scope estimate: ~10,000 lines new + ~5000 lines tests +
~2000 lines migrator. Realistically **4-6 weeks** of focused
implementation with reviews between phases.

---

## 9. Reverting v1.25.0 work?

Phase BACKEND-API-INDEX-OPS (v1.25, merged) added `Rebuild` for
maildir + dbox via `Scan` of the on-disk store. That work was
built ON TOP of yarilo's non-Dovecot dbox format and yarilo's
non-Dovecot fileindex format. It will continue to work after
this compliance phase, but the behaviour changes:

- After Phase 3 (sdbox rewrite), `Scan` for dbox returns
  Dovecot-format records with UID in the filename. The
  reconcile-by-filename logic in backend-api becomes simpler
  (filename → UID is the identity, no lookup needed).
- After Phase 2 (fileindex rewrite), `ResetFolder` writes the
  new Dovecot-compliant layout. No API change.

No reverts needed; the API stays.

The parked `feat/phase-mdbox-prod-ready-a` branch's mdbox work
gets discarded (already noted in its commit message). The
`pkg/locks` COUNTER-INC work on that branch is **still useful**
for the new mdbox in Phase 5 — file_id allocation lives in the
map under map atomic lock, but session-local counters and rate
limiters still benefit. Cherry-pick the COUNTER-INC commit into
a small follow-up PR independent of this compliance plan.

---

## 10. Open questions for reviewer

1. **Production data exposure.** Are there any deployments with
   actual user mailboxes today? Affects migrator complexity
   (best-effort vs. correctness-critical).

2. **Bidirectional format support.** Should yarilo readers
   handle BOTH yarilo-legacy and Dovecot-compliant during a
   transition window, or freeze the legacy reader as a separate
   package only the migrator uses? My recommendation:
   **separate package, no bidirectional reader**.

3. **`.names` sidecar replacement.** Phase 2 needs a decision:
   (a) drop entirely and rely on Maildir filename / sdbox
   filename / mdbox map_uid being self-sufficient; (b) move
   into a proper Dovecot `cache` file (`dovecot.index.cache`);
   (c) keep yarilo-specific as a transition. My recommendation:
   (a) — Dovecot does not need this file.

4. **Major version bump.** This is `v2.0.0`. Confirm the
   semver discipline: any helm release that includes Phase 2+
   must bump to 2.x.y, since per-folder index format changes
   even if migrator handles it.

5. **Maildir compliance audit.** This doc excludes Maildir, but
   yarilo's `yarilo-uidlist` v3 file differs from Dovecot's
   `dovecot-uidlist` v3 in field semantics (we add GUID to
   header). Should there be a Phase 7 for Maildir compliance, or
   is yarilo-uidlist's divergence acceptable / documented?

6. **INTERNALS.md corrections.** §7 (FileIndex) and §8 (dbox)
   contain inaccuracies (most notably the dbox header UID slot
   description in §8). Should they be corrected as part of
   Phase 1, or as a standalone docs PR before any phase lands?

7. **Test fixture sourcing.** Phase 1 needs Dovecot-produced
   `.index` / `.log` / `.map.index` fixtures for parity testing.
   Given Scenario C is the primary use case, these fixtures are
   load-bearing — they ARE the acceptance contract. Options:
   (a) commit binary fixtures into the repo (small — ~10 KB per
   fixture, ~50 KB total once we cover the common shapes);
   (b) generate fixtures at test setup via a Docker-run
   Dovecot. My recommendation: **both** — (a) for unit-test
   regression coverage (fast, deterministic, no Docker), (b)
   as an integration test that proves we stay current as
   Dovecot evolves (catches new minor-version fields).

8. **`pkg/mailindex` vs `internal/storage/mailindex`.** Should
   the new library live in `pkg/` (publicly importable, signals
   "stable API") or `internal/` (yarilo-only)? My recommendation:
   `internal/storage/mailindex` for now — promote to `pkg/` if
   external users emerge.

---

## 11. What I will NOT do without reviewer sign-off

- Start any code work on any phase.
- Touch the parked branch `feat/phase-mdbox-prod-ready-a` (it
  stays parked until we cherry-pick COUNTER-INC into a small
  independent PR).
- Modify INTERNALS.md (the inaccuracies are real but the fix
  belongs after the design is signed off, so we don't have to
  re-edit it as the design shifts during review).
- Make any cross-cutting decisions on the eight open questions
  in §10.

What I WILL do once this PR is reviewed and merged:
- Open Phase 1 PR (pkg/mailindex foundation).
- Track each phase against this doc; update §7 and §10 as
  decisions land.
- Keep the parked branch alive until COUNTER-INC is cherry-picked.
