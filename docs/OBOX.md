# OBOX — object-storage mailbox backend (design)

**Status: planned.** This document is the design foundation for an obox-style
mailbox backend that stores mail on S3-compatible object storage instead of a
POSIX/NFS filesystem.

## Why this is a from-scratch design, not a port

Unlike `director` (open source, ported to `internal/cluster/ring`) and
`fts-flatcurve` (open source, wrapped by `internal/fts/flatcurve`), the Dovecot
**obox** mailbox format and its **`fs-s3`** object-storage driver are **not in
the open-source tree**. Verified against a full `dovecot-2.3.21.1` checkout (the
last 2.3 release, 2024-08-14):

- No `src/plugins/obox`.
- `src/lib-fs/` registers exactly six drivers — `posix`, `dict`, `metawrap`,
  `randomfail`, `sis`, `sis-queue` — and **no `s3`**.

obox + `fs-s3` are OX / Dovecot Pro commercial plugins; only their configuration
and behaviour are documented publicly, never the implementation. So we implement
fresh, taking:

- the **open-source `lib-fs` abstraction** (`src/lib-fs/fs-api.h`) as the
  contract reference for a pluggable object-storage layer, and
- open-source object-storage mail designs (Stalwart, Apache James) as prior art
  for the mailbox format that sits on top.

## Layering

```
  IMAP / LMTP / POP3 session
        |
        v
  MailboxBackend (pkg/mailbox)          ← existing contract, unchanged
        |
        v
  obox mailbox format  (NEW, ours)      ← messages as objects + index in object store
        |
        v
  ObjectFS abstraction (NEW, ours)      ← modelled on lib-fs/fs-api.h
        |
        v
  s3 driver | posix driver | ...        ← concrete backends
```

The **ObjectFS abstraction** is the reusable, domain-free layer this document
specifies. The **obox mailbox format** (how messages, indexes and metadata map
onto objects) is a separate design, layered on top; it is where the real work
is, since the Dovecot format is proprietary.

## ObjectFS contract (extracted from `lib-fs/fs-api.h`)

The lib-fs model is three handle types plus a capability set. It is deliberately
**object-oriented, streaming, and async-capable** — the shape any S3 binding
needs.

### Handle model
- **FS** — a backend instance (driver + args + settings). Ref-counted.
- **File** — a handle to one object at a path. Opened with a *mode*.
- **Iter** — a lazy listing over a path prefix.
- **Lock** — an advisory lock on an object (optional per backend).

### Open modes (`fs_open_mode`)
`READONLY`, `CREATE`, `CREATE_UNIQUE_128` (content-addressed unique name),
`REPLACE`, `APPEND`. Object stores are write-once per key, so `REPLACE` /
`CREATE_UNIQUE_128` are the natural mail-write modes; `APPEND` is emulated or
unsupported.

### Operations
| Group | Ops | Notes |
|:---|:---|:---|
| Read | `read` (buffer), `read_stream` (istream), `prefetch` | streaming for large messages |
| Write | `write` (buffer), `write_stream` + `finish`, `write_set_hash` (MD5/SHA256) | hash on write for integrity / dedup |
| Existence | `exists`, `stat` (size/mtime), `get_nlinks` | `stat` may be served from a listing |
| Delete | `delete` | |
| Copy / move | `copy` (server-side fastcopy when `FASTCOPY`), `rename` | S3 CopyObject = fastcopy |
| List | `iter_init` / `iter_next` / `iter_deinit` | flags: `DIRS`, `ASYNC`, `OBJECTIDS`, `NOCACHE` |
| Lock | `lock(secs)` / `unlock` | advisory; optional |
| Metadata | `set_metadata` / `get_metadata` / `lookup_metadata` | maps to S3 object metadata / tags |

### Async model
- `FS_PROPERTY_ASYNC` advertises non-blocking I/O.
- Async ops return "not finished" and re-drive via `*_finish_async`
  (`fs_write_stream_finish_async`, `fs_copy_finish_async`) plus a per-file
  async callback; `fs_wait_async` blocks for completion.
- For Go this collapses cleanly into `context.Context` + normal blocking calls
  on goroutines; we do **not** need to reproduce the callback/ioloop machinery,
  only the *operation set* and the *capability negotiation*.

### Capability negotiation (`fs_properties`)
A backend advertises what it supports so the layer above adapts:
`METADATA`, `LOCKS`, `FASTCOPY` (+`FASTCOPY_CHANGED_METADATA`), `RENAME`,
`STAT`, `ITER`, `RELIABLEITER` (listing returns the full, current set — **not**
guaranteed on eventually-consistent stores), `DIRECTORIES`, `WRITE_HASH_MD5`,
`WRITE_HASH_SHA256`, `COPY_METADATA`, `ASYNC`, `OBJECTIDS`.

`RELIABLEITER` and the lack of `APPEND` are the two properties that matter most
for S3: listings can be stale under eventual consistency, and objects are
immutable, so the mailbox format must not assume list-after-write consistency or
in-place appends.

## Proposed Go surface (sketch, not final)

```go
// package storage/objectfs (name TBD)

type Capabilities uint32 // METADATA | LOCKS | FASTCOPY | RENAME | STAT | ITER | RELIABLEITER | OBJECTIDS | ...

type FS interface {
    Caps() Capabilities
    Open(path string, mode OpenMode) (File, error)
    Iter(ctx context.Context, prefix string, flags IterFlags) (Iter, error)
    Close() error
}

type File interface {
    Read(ctx context.Context) (io.ReadCloser, error)
    Write(ctx context.Context, r io.Reader, opt WriteOptions) error // opt: hash, metadata
    Stat(ctx context.Context) (Stat, error)
    Exists(ctx context.Context) (bool, error)
    Delete(ctx context.Context) error
    Copy(ctx context.Context, dst string) error   // server-side when FASTCOPY
    Metadata(ctx context.Context) (map[string]string, error)
    Path() string
}

type Iter interface {
    Next() (entry string, ok bool, err error)
    Close() error
}
```

Concrete drivers: `s3` (AWS SDK / MinIO), `posix` (parity + tests without a
bucket). The mailbox format consumes only this interface, so it is testable
against `posix` and swappable to `s3` by config — the same config-not-binary
principle as the rest of yarilo.

## Out of scope for this document
- The obox **mailbox format** itself (message-object layout, index/metacache in
  the object store or a sidecar dict, per-mailbox listing, expunge/refcount) —
  a separate design, the bulk of the OBOX phase.
- Local **fs-cache** (hot-object cache to cut round-trips) — a later
  optimization.
- Eventual-consistency reconciliation strategy — depends on the format above.

## References
- Open source: `dovecot-2.3.21.1/src/lib-fs/fs-api.h` (the abstraction), lib-fs
  drivers (`fs-posix.c`, `fs-dict.c`, `fs-metawrap.c`).
- Behaviour docs (config only, no source): doc.dovecot.org obox / mail_location.
- Prior art (open source): Stalwart, Apache James object-storage mail stores.
