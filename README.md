# yarilo

<table><tr>
<td><img src="https://raw.githubusercontent.com/0kaba0hub/yarilo/main/docs/icon.svg" width="180" alt="yarilo logo"/></td>
<td>

Production-grade IMAP / POP3 / LMTP / ManageSieve / Submission mail server written in Go.
Multi-binary architecture — each protocol component is a separate process. Kubernetes-native via Helm.

[![CI](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml/badge.svg)](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-blue)](https://github.com/0kaba0hub/yarilo)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPLv3-blue.svg)](LICENSE)
[![Status: alpha](https://img.shields.io/badge/status-alpha-yellow)](PLAN.md)

</td>
</tr></table>

---

## Architecture

Yarilo is a **multi-binary** server. Each protocol and infrastructure role is a separate compiled binary — no mode flags, no combined processes. Deployment topology is configured purely through Helm values; the same binaries serve standalone and clustered installations.

```
  Internet
     |
     | IMAPS :993 / IMAP :143 / POP3S :995 / POP3 :110
     | LMTP :24 / ManageSieve :4190 / SASL :4190
     v
+------------------------+  +------------------------+
|  yarilo-imap-login     |  |  yarilo-pop3-login     |  login pods (TLS termination,
|  yarilo-lmtp-login     |  |  yarilo-managesieve-   |  HAProxy / XCLIENT, passdb,
|  yarilo-submission-    |  |  login                 |  anvil rate-limit, fd-passing)
|  login / sasl-login    |  +------------------------+
+------------------------+
         |  Unix fd-passing (SCM_RIGHTS) after auth
         v
+------------------------+  +------------------------+
|  yarilo-imap           |  |  yarilo-pop3           |  session pods (mailbox,
|  yarilo-lmtp           |  |  yarilo-managesieve    |  index, Sieve execution,
|  yarilo-submission     |  |  yarilo-backend-api    |  quota, ACL)
+------------------------+  +------------------------+
         |
         | cross-process write coordination (TCP mTLS)
         v
+------------------------+  +------------------------+
|  yarilo-locks          |  |  yarilo-auth           |  shared services
|  yarilo-anvil          |  |  Redis (dict / locks)  |
+------------------------+  +------------------------+
         |
         | NFS PV (RWX) — shared by all session pods in a tag
         v
  [ Mailbox + Index files ]
```

**Login pods** — terminate TLS (SNI per-domain), authenticate via passdb, enforce per-user connection limits (anvil), pass the raw fd to session pods via SCM_RIGHTS. Stateless; scale independently.

**Session pods** — full mail processing: IMAP / POP3 / LMTP / Sieve / Submission / ManageSieve. Each protocol scales as a separate StatefulSet so IMAP can scale independently of LMTP.

**yarilo-auth** — shared passdb chain (MySQL / Postgres / SQLite / passwd-file / static), auth-token cache, SASL dispatch, master protocol for userdb lookups.

**yarilo-locks** — cross-process write coordination. TCP mTLS `:9104`, Redis-backed state. All Kubernetes deployments (standalone and backend) use remote mode — embedded Unix-socket mode is reserved for unit tests and single-process CLI runs.

**yarilo-anvil** — connection rate limiting and penalty tracking. Shared across all login pods.

---

## Protocol support

| Protocol | Standard | Extensions | Status |
|:---|:---|:---|:---|
| IMAP4rev2 | RFC 9051 | IDLE, MOVE, CONDSTORE, QRESYNC, UIDPLUS, UNSELECT, NAMESPACE, QUOTA, ACL, BINARY, SORT, THREAD, ESEARCH, NOTIFY, URLAUTH, SPECIAL-USE, ID, OBJECTID, METADATA | ✅ |
| POP3 | RFC 1939 | STLS, UIDL, CAPA, XCLIENT | ✅ |
| LMTP | RFC 2033 | per-recipient status, HAProxy, XCLIENT, STARTTLS, `Delivered-To`, Sieve delivery | ✅ |
| ManageSieve | RFC 5804 | full script management | ✅ |
| Sieve | RFC 5228 | fileinto, reject, ereject, envelope, encoded-character, variables, relational, copy, subaddress, environment, body, vacation, vacation-seconds, regex, date, index, editheader, mailbox, mailboxid, duplicate, ihave, special-use, imap4flags, fcc, include, enotify, spamtest, spamtestplus, virustest, foreverypart, mime, extracttext, replace, enclose, mboxmetadata, servermetadata, imapsieve, vnd.yarilo.debug, vnd.yarilo.environment, vnd.yarilo.pipe, vnd.yarilo.filter, vnd.yarilo.report, vnd.yarilo.execute | ✅ |
| Submission | RFC 6409 | STARTTLS, SASL PLAIN, SIZE, PIPELINING, relay to upstream MTA | ✅ |
| SASL | — | PLAIN, LOGIN, SCRAM-SHA-256, XOAUTH2, OAUTHBEARER | ✅ |
| JMAP | RFC 8620/8621 | — | planned |

---

## Storage

| Layer | Backend | Status |
|:---|:---|:---|
| Mailbox | Maildir | ✅ |
| Mailbox | sdbox (single-file dbox with GUID metadata) | ✅ |
| Mailbox | mdbox (multi-message dbox, higher density) | ✅ |
| Mailbox | obox (S3-compatible object storage) — see [docs/OBOX.md](docs/OBOX.md) | planned |
| Index | FileIndex (binary mail-index v7.3 wire format, `.index` / `.index.log` / `.index.names`) | ✅ |

All index mutations go through the cross-process mailbox lock (`yarilo-locks`). Sessions sharing a pod serialise on an in-process `sync.RWMutex` — the Redis lock is only ever contested across pods, not within a single pod.

**Maildir sync-on-open** (`storage.maildir_sync_on_select`, default `true`): on SELECT/EXAMINE, STATUS and IDLE the Maildir index is reconciled against `cur/` and `new/`, so a message delivered by an external MDA or moved/renamed by a second MUA appears without an operator rebuild — files in `new/` are migrated to `cur/`, new files gain a UID, vanished files are expunged, and out-of-band flag renames keep their UID. It is gated on a cheap `cur/`+`new/` mtime token, so a quiescent folder costs a single `stat`. Only Maildir honours it; dbox stays index-authoritative.

**dbox reactive rebuild** (`storage.dbox_reactive_rebuild`, default `true`): dbox is index-authoritative and does not scan storage on every access, but it self-heals reactively. When an **sdbox** read hits a missing or corrupt message file the folder is flagged (a persisted FSCKD marker in the index header); the next SELECT, STATUS or IDLE poll then heals the index under the mailbox lock — every record whose file has vanished is expunged (with a QRESYNC tombstone), surviving messages keep their UID. Transient I/O errors (EIO, timeouts) do not trigger a heal.

**mdbox** now self-heals reactively too, the same way as sdbox: a read that trips over a missing/corrupt message flags the folder FSCKD, and the next SELECT/STATUS/IDLE poll (or POP3 login) runs a targeted per-folder heal under the mailbox lock — every index record whose message is no longer present in storage is expunged (QRESYNC tombstone), the rest keep their UID. This is per-folder and does *not* touch the shared map's refcounts, so it needs none of the quiescence the storage-wide rebuild does. Because it goes through the shared `box.Scan`, a **concurrent `purge`/`altmove`** (both write the new `m.<N>` before unlinking the old) makes the scan incomplete and the heal **abort-and-retry** rather than mistake a just-compacted message for a vanished one — so if a folder stays FSCKD, check whether a purge is running. A structurally corrupt `m.<N>` record also aborts the heal until an operator moves the bad file aside, and the vanished message's map refcount is not decremented by the heal (a leak the next operator rebuild + purge reclaims).

For the **operator** side, because mdbox messages are shared across folders through the map, a per-folder rebuild would import unrelated messages — so the per-folder endpoint (`/api/backend/index/rebuild`) returns `501` for mdbox and there is a dedicated **storage-wide rebuild**: `POST /api/backend/index/rebuild-storage {user}` (or `yarctl backend index rebuild-storage <user>`). It reconciles the shared map against the physical `m.<N>` files under the storage lock, resets every folder index to the messages that still exist, **recomputes each map record's refcount from the actual folder references** (Dovecot `rebuild_apply_map` parity), drops map records whose message vanished, and bumps a persisted `rebuild_count` generation counter in the map header. It **refuses** to run when the scan is incomplete (a half-corrupt `m.<N>` or transient I/O — move/repair the named file and re-run) or when a configured alt tier is unmounted (would mass-expunge alt-resident mail). It is an operator repair tool (Dovecot `force-resync` parity) and should run with the user's mailboxes **quiesced** — no concurrent delivery *and* no concurrent folder operations (CREATE/DELETE/RENAME); neither the delivery folder-append nor the restore is serialised on the storage lock, so a concurrent message or folder change can race the rebuild.

A message present on disk but referenced by no folder becomes **zero-ref** (reported as `unreferenced_zeroref` and logged) so the next purge reclaims it — by default it is **not** re-filed into a mailbox.

Each mdbox message records the mailbox it was saved into (an `ORIG_MAILBOX` key in the dbox trailer, appended after the GUID/size fields — an older reader ignores the unknown key). With **`restore_orphans=true`** (`--restore-orphans` on the CLI) the rebuild re-files an unreferenced message that carries this tag back into its home folder (created if missing), via the normal append path so vsize/modseq/quota stay correct; untagged records are still left zero-ref. A restored orphan comes back with **default flags** (unseen, no keywords) — flags live in the fileindex and an orphan has no index record to recover them from (an accepted gap, like the VANISHED-tombstone gap of the reset path). Restore is off by default and per-request, because the tag proves only *"was once in this folder"*, not *"is lost"* — a delete-with-refcount-leak carries the same tag, so the resurrection decision stays the operator's. A default run (`restore_orphans=false`) writes and reads the tag but takes no action, so its numbers are identical to a rebuild without the feature.

The rebuild **preserves each surviving record's own modseq** across the reset (no QRESYNC modseq storm on an operator rebuild); `highest_modseq` advances to the greatest value carried in, and only a record with no modseq (a freshly assigned UID) is stamped fresh. It also **notifies FTS to expunge** every UID it drops, per folder — the storage-wide rebuild, the per-folder rebuild, and the reactive heal all return the dropped UIDs so their FTS documents are invalidated immediately instead of lingering as ghost entries until the next `fts rescan` (the POP3-only reactive heal has no FTS client wired in, so those still rely on the next rescan). One accepted gap remains: the reset still loses per-UID VANISHED tombstone fidelity for dropped records, since it rewrites the whole set.

On-disk `m.<N>` files follow the **dbox v2 layout**: the ASCII file-header line (`version M<hdr-size> C<create-stamp>`) is written **once per physical file**, before its first message, then each message is `[32-byte header][body][trailer]`. The reader is self-describing — at each record it tells a file-header line (starts with the ASCII version digit) apart from a raw message header (starts with the `\x01\x02` magic) by the first byte, so a real Dovecot instance parses past the first message in a multi-message file, and legacy yarilo stores that stamped the header before every record still read back unchanged (no migration).

Three Dovecot-parity knobs tune when a new `m.<N>` is rolled and how it is allocated (all under `storage:`, mdbox only):

| Key | Default | Effect |
|:---|:---|:---|
| `mdbox_rotate_size` | `10485760` (10 MiB) | Max bytes per `m.<N>` before the next save rolls to a fresh file. `0` selects the 10 MiB default. |
| `mdbox_rotate_interval` | `0` (disabled) | Seconds; roll the append file once it is older than this, regardless of size. |
| `mdbox_preallocate_space` | `false` | `fallocate()` the new file to `mdbox_rotate_size` up front (Linux only; a no-op elsewhere). |

The age check reads a **persisted per-file create-time** stored in the map header (not a filesystem `btime`, which is unreliable over NFS), so it survives restarts. Unlike Dovecot it uses a **rolling window** (`now − createTime > interval`) rather than a clock-boundary snap, so "rotate every interval" means the file actually lived at least that long. Preallocation uses `FALLOC_FL_KEEP_SIZE` so the file's logical size still grows from zero as records are appended — reserving blocks without breaking the offset model — and any failure is a non-fatal hint.

The **reactive heal** is retry-bounded per folder per session on the IMAP path: a near-continuous purge/altmove keeps every scan incomplete (the heal aborts rather than mistake a compacted message for a vanished one), so after a few consecutive aborts the session stops auto-retrying that folder — each attempt costs a full storage scan — and logs once, pointing the operator at a rebuild. The counter resets on a successful heal or when another session clears the marker. The **POP3** path carries no such bound: a POP3 session heals at most once, at login, not in a command loop, so it cannot spin; a rapidly reconnecting client during a purge could still reproduce the storm across logins, but POP3 sessions are short and a cross-login bound would need persistent state — an accepted gap.

---

## Full-text search

`SEARCH BODY`, `SEARCH TEXT` and `SEARCH HEADER` are backed by a per-user full-text index instead of a linear message scan. `SEARCH RETURN (RELEVANCY)` (RFC 4731/6203) surfaces the engine's ranking as scores 1-100, min-max normalized per result set — requires a [`yarilo-patches`](https://github.com/0kaba0hub/go-imap/tree/yarilo-patches) go-imap fork, since upstream has no RELEVANCY support.

| Component | Backend | Status |
|:---|:---|:---|
| Engine | flatcurve (Xapian, on-disk glass shards) via [`go-xapian`](https://github.com/0kaba0hub/go-xapian) | ✅ |
| Indexer / lookup service | `yarilo-fts` (sole writer; sessions dial it) | ✅ |

The **`yarilo-fts`** service owns the index end-to-end — indexing *and* lookups — so it is the only process that links libxapian (cgo); every session binary stays pure-Go and sends `LOOKUP` over the internal TAB protocol. Enable it with `fts.enabled` + `fts_engine: flatcurve`; startup fails fast on a missing or unknown engine.

**Multi-language** (`languages: [en, de, ...]`, deliberately asymmetric): indexing auto-detects exactly ONE language per body/attachment part and applies only that part's chain (falling back to the first configured language on short/ambiguous text); search expands every query token through EVERY configured language's stemmer, OR'd together, so a query matches content no matter which configured language it was indexed under. Stemmers/stopwords cover en/fr/de/it/pt/ru/es. **Ukrainian (`uk`)** is also configurable — there is no official Snowball stemming algorithm for Ukrainian (the reference implementation cannot stem it either), so `uk` gets lowercase + stopwords only, never stemming; it's still auto-detected and never mis-routed to a neighbouring language's chain (e.g. `ru`) in a mixed-language mailbox. `fts_language_filters_override` (per language, e.g. `{uk: [lowercase, stopwords]}`) replaces `language_filters` for specific languages without affecting the rest — a full replacement, not a merge; every key must also be a configured language. `fts_language_tokenizer_generic_token_maxlen` / `fts_language_tokenizer_address_token_maxlen` tune the tokenizer's byte caps (0 = defaults 30/250). `fts_language_tokenizer_generic_algorithm` accepts `simple` (implemented) or `tr29`/`wb5a`/`explicit_prefix` (accepted keys, reject at startup — blocked on the Bleve stream, not silently ignored). `fts_search` (default `true`) disables SEARCH only, keeping indexing/autoindex running — an incident-response toggle distinct from `fts.enabled`.

Messages are indexed **write-through at delivery** (the body is already in memory) and via **autoindex** on access; a per-mailbox checkpoint (`last_indexed_uid` + a settings checksum) drives incremental indexing and detects config drift for rebuilds. A UIDVALIDITY change resets the checkpoint so a recreated mailbox re-indexes cleanly. The fts-flatcurve directory lives inside the mailbox's own driver-aware index path, matching where the mailbox index sits (`…/mailboxes/<folder>/dbox-Mails/fts-flatcurve` for mdbox/sdbox). Large mailboxes rotate the index into sealed shards; SEARCH queries each shard and merges, so results stay correct past the rotate threshold. Once a mailbox's sealed-shard count reaches `fts_flatcurve_optimize_limit` (default 10, `0` disables), it's automatically queued for background compaction back to a single shard — no manual `yarctl fts optimize` needed, though that command still works for an on-demand whole-user compaction. When the index is unavailable, `fts_search_read_fallback` decides whether a query silently falls back to a sequential scan or surfaces a hard error (set `false` to catch regressions loudly).

**Migrating from `dovecot.conf`** (see docs/FTS.md §13 for the full list): `fts_message_max_size: 0` means unlimited here — the reference rejects a literal `0` and spells "no limit" as `unlimited`, so translate that keyword to `0` rather than copying either value across literally. Time values are bare integers with the unit fixed in the key name (`fts_search_timeout_secs`, `fts_flatcurve_rotate_time` in ms) instead of the reference's `"30s"`-style TIME strings. `fts_decoder_driver: none` is yarilo's "disabled" spelling (an empty string is also accepted). `fts_decoder_script_addr` is a superset of the reference's plain socket path — also accepts `host:port` for a standalone decoder Deployment/Service.

**Acceptance benchmark** (`app/fts-bench`, synthetic corpus, local Xapian glass shards):

| Corpus | Shards | Index size | Index rate | SEARCH p95 (indexed vs scan) |
|:---|:---|:---|:---|:---|
| 5,000 | 1 | 1.59× corpus | 9,654 msg/s | 0.08 ms vs 77.7 ms (**942×**) |
| 10,000 | 2 | 1.62× corpus | 9,764 msg/s | 0.14 ms vs 149 ms (**1,090×**) |
| 20,000 | 4 | 1.63× corpus | 10,027 msg/s | 0.23 ms vs 322 ms (**1,410×**) |

Search stays sub-millisecond as the mailbox grows; the linear scan it replaces grows with message count. See `docs/FTS.md` for the full design and the phased roadmap (relevancy / strict-substring / multi-language, then attachment decoders).

**Attachment text extraction** (`fts_decoder_driver`, opt-in, default `none`): no built-in PDF/office parsing — extraction is delegated to an external `script` decoder (a TAB-delimited line protocol over `fts_decoder_script_addr`, either `unix:///path.sock` for a co-located process or `host:port` for a standalone Deployment/Service) or an Apache Tika server (`fts_decoder_tika_url`). HTML parts are always tag-stripped in-process, independent of the decoder. The `script` driver may advertise its supported content-types/extensions once via an optional `TYPES` query (cached for the process lifetime) — parts outside that list are skipped locally without dialing; a decoder that doesn't recognize `TYPES` falls back to asking per part, unchanged from before. The `tika` driver sends the attachment's filename via `Content-Disposition` and retries connection errors/5xx responses (`fts_decoder_max_attempts`, default 2); once retries are exhausted the message indexes without that attachment's text (`fts_decoder_degraded_total`) instead of failing outright, while a hard failure (a permanent 4xx, bad config) aborts the message so autoindex retries it later. **Within-message dedup** (`fts_dedup_body_parts`, opt-in, default `false`) skips re-tokenizing a body part whose normalized text was already indexed for the same message — `multipart/alternative` text+html twins, or a quoted block repeated within one body — without buffering or hashing on the default fast path when disabled. Cross-message dedup is out of scope: it cannot be done without breaking per-message search correctness.

**Per-part language detection** (`languages`, 2+ entries): each body/attachment part picks its own stemmer from its own text — a German reply quoted inside an English thread, or a French PDF attached to an English message, indexes correctly under its own language instead of the whole message's. Header values (addresses, message-ids, subjects) are never stemmed — they're tokenized and lowercased only, since search already matches them via the raw query-token variant. `fts_detection_sample_bytes` (default 1024) and `fts_detection_min_runes` (default 10) tune how much of a part is sampled and how much text is required before trusting the detector, growing the sample once before falling back to the first configured language.

---

## Cluster components

| Binary | Role | Status |
|:---|:---|:---|
| yarilo-imap | IMAP4rev2 session server | ✅ |
| yarilo-imap-login | IMAPS / IMAP login proxy — TLS termination, passdb, fd-passing | ✅ |
| yarilo-pop3 | POP3 session server | ✅ |
| yarilo-pop3-login | POP3S / POP3 login proxy | ✅ |
| yarilo-lmtp | LMTP delivery server (Sieve, quota) | ✅ |
| yarilo-lmtp-login | LMTP login proxy — HAProxy, XCLIENT, preamble strip | ✅ |
| yarilo-managesieve | ManageSieve script management server | ✅ |
| yarilo-managesieve-login | ManageSieve login proxy — STARTTLS, HAProxy | ✅ |
| yarilo-submission | Submission relay server | ✅ |
| yarilo-submission-login | Submission login proxy | ✅ |
| yarilo-sasl-login | SASL auth socket for Postfix / Exim relay | ✅ |
| yarilo-auth | Passdb chain, auth cache, SASL dispatch, master userdb | ✅ |
| yarilo-anvil | Connection rate limiting + penalty | ✅ |
| yarilo-locks | Cross-process write coordination — Redis-backed, TCP mTLS | ✅ |
| yarilo-quota-status | Quota policy socket (Postfix quota check) | ✅ |
| yarilo-fts | Full-text search indexer + lookup service (flatcurve/Xapian; sole cgo/libxapian process) | ✅ |
| yarilo-backend-api | HTTP admin API (dict, ACL, folder, quota, rebuild) | ✅ |
| yarctl | CLI control tool — `director` and `backend` planes (backward-compat alias: `yarilo-admin`) | ✅ |
| yarilo-monitor | Backend health sidecar for director ring | ✅ |
| yarilo-migrate | Offline mailbox migration (Maildir ↔ dbox ↔ mdbox) | ✅ |
| yarilo-director | Consistent-hashing ring, sticky sessions, failover | planned |

All intra-cluster protocols are TAB-delimited text with LF termination and a version handshake.
Full wire-format specification: [INTERNALS.md](INTERNALS.md).

Every login proxy (imap/pop3/submission/managesieve/lmtp) routes sessions one of two ways: `backend_addr` (standalone, a fixed backend) or `director_addr` (director mode, per-session `LOOKUP` via yarilo-director) — at least one is required, and `backend_addr` wins when both are set (#735, unified across all five components including lmtp-login in #741). `backend_port` overrides the port a director `LOOKUP` returns when it differs from the backend's protocol-specific containerPort.

**Migration note (#741):** lmtp-login previously had the *opposite* precedence — `director_addr` won when both were set. If your `lmtpLogin` Helm values set both `backend_addr` and `director_addr`, the login pod now logs a startup warning and silently switches from director routing to the static backend. Remove `backend_addr` from `lmtpLogin` values to keep director routing.

`director_service.username_hash_lowercase` (default `true`, Helm: `components.director.username_hash_lowercase`) lowercases usernames before they're hashed for ring routing or used as keys for sticky assignments and admin (`USER-MOVE`) overrides (#738) — without it, two spellings of the same account (`User@d.test` / `user@d.test`) can hash to different values and land on different backends, defeating sticky routing. Migration note: enabling this on an already-running cluster changes hashes for mixed-case usernames — their existing sticky entries just expire naturally via `user_expire`, no special migration step is needed.

Every director `LOOKUP` carries a mandatory tag field — there is no full-ring mode (#737): `""` selects the untagged backend pool, not "any tag." Two sharding models are supported:

- **Static (dedicated login fleet per tag pool).** Each login component's `director_tag` (Helm: `components.<login>.director_tag`, e.g. `components.imapLogin.director_tag`) restricts that component's lookups to one tag-pool — set this when running a dedicated login Deployment per tag pool, per `docs/DEPLOYMENT.md`'s tag-based sharding model. In a deployment with no tags configured at all, every backend is untagged, so the default `director_tag: ""` behaves exactly like the old (buggy) full-ring lookup — untagged/standalone deployments see no behavior change.
- **Shared (per-user tag from passdb/userdb, #746).** One login fleet serving users of every tag-pool: a `director_tag` extra field on the passdb or userdb response (SQL: an ordinary column in `password_query`/`user_query`, same generic column-forwarding as `allow_nets` — no driver code change needed) picks the tag for that one user's `LOOKUP`, overriding the component's static `director_tag`. IMAP/POP3/Submission/ManageSieve pick it up from the AUTH response; LMTP resolves it with a per-recipient userdb lookup before the director `LOOKUP`. A user with no `director_tag` field falls back to the component's static value.

Director replicas self-organize into a ring at runtime (#750 phase 1 — replaces the earlier static full-mesh `peers` list, #700): members are ordered by `(ip, port)` and each dials only its right neighbor, never a full mesh. Every member count is a fully valid, service-serving state (never refuses service) — a lone director is an ordinary N=1 ring, no peer machinery runs at all. `components.director.peers` is now a **seed list**: each entry is tried in turn for a one-time `DIRECTOR-JOIN`, after which membership maintains itself via propagation. Left empty (the default), at `replicas > 1` the seed auto-derives to the headless `<release>-director-ring:9102` Service (#751/#764) — never the ClusterIP `<release>-director` Service — because only the headless name resolves directly to every ready pod's IP, which is what the DNS fan-out needs to poll each peer; the ClusterIP resolves to a single virtual IP that load-balances dials randomly and reintroduces the formation partition. An explicit list overrides this (non-k8s / manual seeding). `components.director.ring_secret` (auto-generated into Secret `<release>-director-ring-secret`, mirroring the API token) authenticates joins via HMAC-SHA256 — leaving it unset rejects every join attempt outright, so that replica can only ever run standalone. `components.director.min_members` (default 3) is an install-time warning only, no runtime effect. Phase 1 covers ring topology, membership propagation, and the HMAC join core; dial-back verification + CIDR filtering (phase 2), full user/backend state snapshot on connect (phase 3), and members_hash anti-entropy (phase 4) are tracked separately.

**#754 (found in the first live 3-replica sandbox test)** fixed a phase-1 regression where killing one ring member left the membership set permanently corrupted: a dead member's tombstone wasn't propagated (an ordering bug meant `DIRECTOR-REMOVE` was announced before the outgoing connection needed to send it existed) and, separately, a plain member-list union on every reconnect could silently resurrect a member some other node had already correctly evicted. Membership now carries a proper tombstone set, exchanged alongside the member list on every ring connection; `Member` ordering also now sorts by parsed IP octets instead of string comparison (`"10.0.0.17" < "10.0.0.6"` as strings, backwards from the real address).

**#755** fixed `yarctl director ...` returning 403 from every pod, including the director pod's own shell — the director admin API token and URL were never plumbed to `yarilo-backend-api` (the standard admin plane) or reliably available for local use. Both are now wired via a shared Helm env-injection helper, and the empty-by-default `api.allowed_nets` (see #759) means the bearer token is the sole gate rather than a cluster-specific CIDR. The `smoketest` binary now carries a `-director-api <url>` check (bearer token from `-director-api-token` or `DIRECTOR_API_TOKEN`/`YARILO_ADMIN_TOKEN`) that fails loudly on a 403/401 and asserts a member list on 200 — run it in-cluster (a Job or `kubectl exec` on `yarilo-backend-api`, where the token env is already injected) since the director API is a ClusterIP; `smoke.yml` gained an optional `director_api` input for the same check from an in-cluster runner.

**#758** replaced the ring's single-edge event-forward path (one connection picked at accept time — `dialConn`, or a `passiveConn` reserved for the N=2 tie-break's passive member) with a broadcast to every currently live ring connection except whichever one an event just arrived on, matching Dovecot's `director_update_send` skip-arrival model instead of a fixed per-connection role that could go stale mid-connection across an N=3→N=2 shrink.

**#759 (found in a live 3-pod simultaneous-start sandbox test)** fixed a load-balanced ClusterIP seed routing a pod's own `DIRECTOR-JOIN` dial back to itself: this looked like an ordinary, immediate join success (a self-join is a harmless no-op), so `joinLoop` stopped retrying the seed forever, leaving the pod stuck as a permanently isolated N=1 that never discovered any real peer. `handleJoin` now rejects a self-dial explicitly, so the existing generic retry keeps dialing the seed until kube-proxy routes it elsewhere. Also dropped `components.director.api.allowed_nets`'s hardcoded kubeadm-shaped default (`10.96.0.0/12` + `10.244.0.0/16`) — wrong for any cluster with different service/pod CIDRs, silently 403ing every request including well-authenticated ones — in favor of an empty default (token-only auth; CIDR filtering is opt-in defense-in-depth once an operator knows their real cluster CIDRs).

**#759 follow-up (live re-test on the first fix)** closed the two remaining formation failures. Convergence speed: a self-dial rejection now retries the seed on a short fixed interval (500ms) instead of walking the exponential backoff — the backoff treated an expected ~1/N outcome of a load-balanced seed as seed failure, stretching one pod's convergence past 60s. Lost `DIRECTOR-ADD` under concurrent formation: every membership-changing path now broadcasts over the *pre-reconcile* connection set before recomputing its right neighbor (`reconcile()` could tear down a live connection the announcement still needed, permanently stranding a directly-connected member at a stale view). And a periodic anti-entropy snapshot (`components.director.anti_entropy_interval`, default 3s) re-broadcasts the member+tombstone `DIRECTOR-LIST` over every live ring connection as a bounded safety net — any split with at least one crossing connection heals within one interval.

**#759 second follow-up (live re-test showed formation still partitioning)** closed the last structural gap: fully disjoint subrings with zero crossing connections, which every connection-bound mechanism above is architecturally blind to. Under concurrent formation each node's single right-neighbor dial is computed from its own divergent view, so the dial graph isn't guaranteed connected — and the one guaranteed crossing point (the shared ClusterIP seed) used to be contacted exactly once. The seed poll is now periodic (`components.director.seed_poll_interval`, default 2s; negative restores one-shot): every member keeps re-fetching the seed's member+tombstone snapshot through the same idempotent union merge, bounding any formation partition's lifetime by the poll interval regardless of dial topology. Re-joins from known members are served as read-only snapshot requests (no `DIRECTOR-ADD` storm). Gated in-process by a formation test that joins N members simultaneously through a mock load-balanced seed with random routing (including self-dials).

**#759 third follow-up (live re-test: healing worked but took ~2 minutes, failing the converge-in-seconds gate)** removed the two latency sources. Poll pacing now gates on the configured cluster target size instead of own-view stability: full cadence while the view holds fewer than `min_members`, easing to `seed_poll_idle_interval` once the expected size is reached (and snapping back on any loss) — the previous stable-view backoff slowed exactly the node that needed healing, because a partitioned node's own view looks perfectly stable. And a hostname seed is now resolved explicitly with the poll fanned out to every resolved address except self each cycle: with the headless `-director-ring` Service as the seed this is a deterministic sweep of all peers (convergence in about one poll interval, zero self-dials) and sidesteps Go's RFC 6724 own-IP-first address ordering that a naive dial of a headless DNS answer hits. A literal-IP or load-balanced seed keeps the previous behavior (server-side self-dial rejection + 500ms fast retry) as the fallback path.

**#764 (live breakthrough after the DNS fan-out landed)** made the chart default the ring seed to the headless Service so the fix can't be defeated by configuration. With the fan-out in place, formation converged with the headless `-director-ring` seed (DNS → all pod IPs) but still partitioned with the ClusterIP `-director` seed (DNS → one virtual IP → load-balanced random routing → self-dials). The seed is now auto-derived to `<release>-director-ring:<directorPort>` whenever `components.director.peers` is empty and `replicas > 1`, so a fresh `replicas=3` install converges with no manual seed and no way to accidentally point the ring at the ClusterIP. `-director` remains the login-pod LOOKUP endpoint only.

**#768 (supersedes the brief all-probes-all liveness)** realigned death detection to the reference's both-neighbors model: a death is detected by the dead node's immediate neighbors — O(1) probes per node regardless of ring size — never by an O(N²) everyone-probes-everyone sweep. The right side was already covered by the dial path; the **left side** now treats losing the accepted connection from the current left neighbor as a death signal too (verified with a few short-deadline probes before declaring, so a benign dial re-target isn't mistaken for a death) — previously the N=2 higher-sorted member, who never dials, was permanently blind to its only peer's death. Both ends of every ring connection also exchange `PING`/`PONG` keepalives on `ping_interval` with a `ping_interval + ping_timeout` read deadline, so a silently-hung peer surfaces as a read error feeding the same death paths; and every deliberate teardown announces itself with `QUIT\t<reason>` (reference parity), so a benign dial re-target is classified instantly instead of via probes. Phantom members from rolling-restart churn converge via anti-entropy spreading the phantom until exactly one node computes it as its right neighbor, dials it, fails, and evicts it for everyone. Tombstones carry a TTL (`components.director.tombstone_ttl`, default 600s) so churn across many rollouts can't grow the set unboundedly — safe because a resurrected-but-unreachable member is re-evicted by its neighbors within seconds regardless.

**#772 (userDir state exchange — snapshot base)** starts closing the routing-STATE half of the ring (the membership half being done). On every ring (re)connect, right after the member `DIRECTOR-LIST`, a director now streams its **userDir snapshot** — `USER\t<hash>\t<backend>\t<seq>\t<by>\t<weak>` per sticky assignment — so a fresh or restarted director inherits current sticky routing state immediately instead of starting empty and re-deriving only from the hash. Assignments carry a **Lamport-clock** stamp `(assign_seq, assign_by)`, not wall-clock (pod clocks are unsynchronized; unix-nano would let the fastest-clock replica win nondeterministically) — a strictly higher seq wins, a tie breaks to the lower director id, deterministic and test-reproducible. A fresh sticky assignment made by a normal `LOOKUP` also propagates live around the ring as a `USER-ASSIGN` event (only the new pin — not the sticky TTL-refresh, which would make every repeat login a broadcast), director↔director by hash, applied under the same Lamport order. When a merge moves a user to a different backend — a same-`assign_seq` conflict where the lower id won and this director lost, or a newer reassignment — the loser kicks its own now-stale sessions for that user off the wrong backend (`USER-KICKED` to the owning login proxy), so a mailbox is never split across two backends; other users on that backend keep running. This completes the #772 userDir state exchange: sticky routing is now consistent across replicas even where it diverges from the deterministic hash, closing the replication half of #708 and the divergence part of #706.

**#770 (graceful leave on SIGTERM)** removes the death-detection latency from planned exits. Before shutting down, a director announces `DIRECTOR-REMOVE` for itself around the ring (peers evict it instantly via the existing tombstone path — zero probe window), sends `QUIT` on every ring connection, and rejects further JOINs (`JOIN-FAIL\tshutting down`) so no fresh joiner can learn the dying member. A hard kill still converges via the #768 neighbor-monitoring path — graceful leave only optimizes the expected k8s rolling-restart / scale-down case.

**#765 (residual after the seed fix: a respawned pod held a recently-dead member)** made the settled-cadence backoff safe by default. Killing a pod on a converged ring respawns a fresh pod that could learn the dying member as *live* during the death-detection window, reaching `min_members` with a stale entry — at which point the previous lazy 30s idle cadence left the dead member in place for up to 30s (live-measured 40s+). The idle cadence is now a separate knob `components.director.seed_poll_idle_interval`, **defaulting to the same 2s as the active cadence** (no effective backoff), because a node cannot locally distinguish "converged" from "stable but holding a dead member". Operators can raise it to trade steady-state polling for slower dead-member eviction; the tombstone-wins merge and per-`DIRECTOR-LIST` tombstone exchange (#754) already guaranteed correctness — this only bounds the latency.

---

## Quick start (Helm)

```sh
# Add the chart (local checkout)
helm upgrade --install yarilo ./helm \
  -f helm_values/values-sandbox.yaml \
  -n yarilo --create-namespace
```

See [INSTALL.md](INSTALL.md) for a full Kubernetes walkthrough with cert-manager, Let's Encrypt, and an external MySQL passdb.

Minimal `yarilo.yaml` for bare-metal single-node:

```yaml
hostname: mail.example.com

auth:
  passdb:
    - driver: mysql
      dsn: "yarilo:secret@tcp(127.0.0.1:3306)/yarilo"

storage:
  persistence:
    enabled: true
    size: 50Gi

locks:
  mode: embedded   # single-node only; use remote in k8s
```

```sh
LOG_LEVEL=debug yarilo-imap -config yarilo.yaml
```

`LOG_LEVEL=debug` is **per-service** — set it on the process whose code path you are tracing (a delivery breadcrumb lives in `yarilo-lmtp`, a search one in `yarilo-imap`). At debug level every write path emits an explicit "wrote UID=N file=Q" line (`lmtp: delivered`, `imap: append saved`, `imapsieve: fileinto saved`, `sieve/pipe: invoked`, `sieve/sender: notification sent`), and the read side logs what it actually scanned when a lookup comes back empty (`imap: search matched no messages` with the folder's record count and UID range, `imap: fetch skipped uid absent from client view`, `fileindex: reload applied` / `reset folder` with record counts before/after) — so a delivery→visibility mismatch is diagnosable from the log alone. The **full-text search** pipeline is instrumented the same way: `yarilo-fts` logs every request it handles (`fts: indexed` / `expunged` / `lookup` / `rescanned` / `status`) and the indexing worker reports each run (`fts: index run start` / `done` with checkpoint, indexed/skipped counts and duration, plus per-message `fts: message indexed`), while the `yarilo-imap`/`yarilo-lmtp` sides log the handoff (`imap: fts notify sent`, `lmtp: fts autoindex queued`) and how many candidates a search got back (`imap: fts search candidates`). FTS lines log **result and term COUNTS, never the query terms** (private mail content).

These lines carry only metadata (user, folder, UID, filename, counts); **passwords, tokens, SASL response data, and search query terms are never logged**.

---

## Quick start (Docker Compose)

Single-host, no Kubernetes — the standalone topology (login proxies + session
backends + auth/anvil/locks + Redis) on one host, SQLite userdb, one image:

```sh
cd deploy/compose
cp .env.example .env
./gen-certs.sh mail.example.test     # self-signed TLS for local use
docker compose up -d
```

Full walkthrough — creating users, TLS, MTA (Postfix) integration, verifying,
backups — in [docs/DOCKER-COMPOSE.md](docs/DOCKER-COMPOSE.md).

---

## Mailbox migration

```sh
yarilo-migrate \
  --from /var/mail/vhosts \
  --to   /var/mail/dbox \
  --format dbox   # or mdbox; --dry-run to preview
```

---

## Documentation

| Document | Contents |
|:---|:---|
| [PLAN.md](PLAN.md) | Implementation plan, phases, timelines |
| [INTERNALS.md](INTERNALS.md) | Wire-format specs for all internal protocols |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Code-level architecture, process model, storage contract |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | K8s topology, sizing, HA strategy, sharding via tags |
| [docs/GENERAL.md](docs/GENERAL.md) | `general`: SSL, HAProxy, XCLIENT, connection limits |
| [docs/SERVICES.md](docs/SERVICES.md) | `services`: per-listener config |
| [docs/IMAP.md](docs/IMAP.md) | `protocol.imap`: IDLE, line length, ACL, NAMESPACE, NOTIFY, METADATA, OBJECTID |
| [docs/NAMESPACE.md](docs/NAMESPACE.md) | IMAP namespaces (RFC 2342 / 9051): personal / shared / other_users |
| [docs/SUBMISSION.md](docs/SUBMISSION.md) | `protocol.submission`: hostname, size, relay |
| [docs/LMTP.md](docs/LMTP.md) | `protocol.lmtp`: delivery, HAProxy, XCLIENT, TLS, headers |
| [docs/POP3.md](docs/POP3.md) | `protocol.pop3`: UIDL, soft-delete, migration |
| [docs/SIEVE.md](docs/SIEVE.md) | `sieve`: filtering, ManageSieve, imapsieve, vacation, extensions |
| [docs/AUTH.md](docs/AUTH.md) | `auth.passdb`: SQL / passwd-file / static backends, password schemes, userdb extra fields |
| [docs/QUOTA.md](docs/QUOTA.md) | `quota`: count-authoritative engine, grace, warnings, mail_size, clone mirror, over-status |
| [docs/SMOKE.md](docs/SMOKE.md) | End-to-end smoke test |
| [docs/DIRECTOR.md](docs/DIRECTOR.md) | `director_service`: ring, peers, HAProxy, XCLIENT, mTLS |
| [docs/MONITOR.md](docs/MONITOR.md) | `yarilo-monitor`: health probes, Prometheus metrics |
| [docs/DIRECTOR-API.md](docs/DIRECTOR-API.md) | Director HTTP admin API |
| [docs/BACKEND-API.md](docs/BACKEND-API.md) | Backend HTTP admin API |
| [docs/YARILO-ADMIN.md](docs/YARILO-ADMIN.md) | `yarctl` CLI reference |
| [docs/DICT.md](docs/DICT.md) | `pkg/dict` KV-store abstraction: drivers, YAML schema |

---

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).
