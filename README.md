# yarilo

<table><tr>
<td><img src="https://raw.githubusercontent.com/yarilomail/yarilo/main/docs/icon.svg" width="180" alt="yarilo logo"/></td>
<td>

Production-grade IMAP / POP3 / LMTP / ManageSieve / Submission mail server with built-in full-text search, written in Go.
Multi-binary architecture — each protocol component is a separate process. Kubernetes-native via Helm.

</td>
</tr></table>

<!-- Badges live OUTSIDE the table on purpose. Inside the <td> Chrome laid them
     out one per row while Safari kept them inline, from the same GitHub-rendered
     HTML; as a top-level paragraph they get the full README width and both
     browsers agree. Keep them here, on one line. -->
[![CI](https://github.com/yarilomail/yarilo/actions/workflows/ci.yml/badge.svg)](https://github.com/yarilomail/yarilo/actions/workflows/ci.yml) [![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/) [![Platform](https://img.shields.io/badge/platform-linux%2Famd64-blue)](https://github.com/yarilomail/yarilo) [![License: AGPL v3](https://img.shields.io/badge/license-AGPLv3-blue.svg)](LICENSE) ![Status: beta](https://img.shields.io/badge/status-beta-orange)

---

## Architecture

Yarilo is a **multi-binary** server. Each protocol and infrastructure role is a separate compiled binary — no mode flags, no combined processes. Deployment topology is configured purely through Helm values; the same binaries serve standalone and clustered installations.

**Login pods** — terminate TLS (SNI per-domain), authenticate via passdb, enforce per-user connection limits (warden), pass the raw fd to session pods via SCM_RIGHTS. Stateless; scale independently.

**Session pods** — full mail processing: IMAP / POP3 / LMTP / Sieve / Submission / ManageSieve. Each protocol scales as a separate StatefulSet so IMAP can scale independently of LMTP.

**yarilo-auth** — shared passdb chain (MySQL / Postgres / SQLite / passwd-file / static), auth-token cache, SASL dispatch, master protocol for userdb lookups.

**yarilo-locks** — cross-process write coordination. TCP mTLS `:9104`, Redis-backed state. All Kubernetes deployments (standalone and backend) use remote mode — embedded Unix-socket mode is reserved for unit tests and single-process CLI runs.

**yarilo-warden** — connection rate limiting and penalty tracking. Shared across all login pods.

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
| JMAP | RFC 8620/8621 | session, `Mailbox/{get,query,changes}`, `Email/{get,query,set,changes}`, `Thread/{get,changes}`, `SearchSnippet/get`, blob download, back-references | partial |

---

## Storage

| Layer | Backend | Status |
|:---|:---|:---|
| Mailbox | Maildir | ✅ |
| Mailbox | sdbox (single-file dbox with GUID metadata) | ✅ |
| Mailbox | mdbox (multi-message dbox, higher density) | ✅ |
| Mailbox | obox (S3-compatible object storage) | planned |
| Index | FileIndex (binary mail-index v7.3 wire format, `.index` / `.index.log` / `.index.names`) | ✅ |

All index mutations go through the cross-process mailbox lock (`yarilo-locks`). Sessions sharing a pod serialise on an in-process `sync.RWMutex` — the Redis lock is only ever contested across pods, not within a single pod.

Self-healing (Maildir sync-on-open, dbox/mdbox reactive heal), the operator rebuild path, and mdbox rotation/tuning knobs: see **[STORAGE](https://doc.yarilomail.org/STORAGE)**.

---

## Full-text search

`SEARCH BODY`, `SEARCH TEXT` and `SEARCH HEADER` are backed by a per-user full-text index instead of a linear message scan. `SEARCH RETURN (RELEVANCY)` (RFC 4731/6203) surfaces the engine's ranking as scores 1-100, min-max normalized per result set — requires a [`yarilo-patches`](https://github.com/0kaba0hub/go-imap/tree/yarilo-patches) go-imap fork, since upstream has no RELEVANCY support.

| Component | Backend | Status |
|:---|:---|:---|
| Engine | flatcurve (Xapian, on-disk glass shards) via [`go-xapian`](https://github.com/0kaba0hub/go-xapian) | ✅ |
| Indexer / lookup service | `yarilo-fts` (sole writer; sessions dial it) | ✅ |

The `yarilo-fts` service owns the index end-to-end (indexing *and* lookups) and is the only process linking libxapian (cgo); session binaries stay pure-Go and send `LOOKUP` over the internal TAB protocol. Enable it with `fts.enabled` + `fts_engine: flatcurve`. Multi-language indexing/search, attachment decoders, and the full config surface are documented in **[FTS](https://doc.yarilomail.org/FTS)**.

**Acceptance benchmark** (`app/fts-bench`, synthetic corpus, local Xapian glass shards):

| Corpus | Shards | Index size | Index rate | SEARCH p95 (indexed vs scan) |
|:---|:---|:---|:---|:---|
| 5,000 | 1 | 1.59× corpus | 9,654 msg/s | 0.08 ms vs 77.7 ms (**942×**) |
| 10,000 | 2 | 1.62× corpus | 9,764 msg/s | 0.14 ms vs 149 ms (**1,090×**) |
| 20,000 | 4 | 1.63× corpus | 10,027 msg/s | 0.23 ms vs 322 ms (**1,410×**) |

Search stays sub-millisecond as the mailbox grows; the linear scan it replaces grows with message count. See `https://doc.yarilomail.org/FTS` for the full design and the phased roadmap (relevancy / strict-substring / multi-language, then attachment decoders).

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
| yarilo-jmap-login | JMAP login proxy — TLS termination, auth, warden, HTTP proxy | ✅ |
| yarilo-jmap | JMAP backend — session resource, RFC 8620/8621 methods | ✅ |
| yarilo-sasl-login | SASL auth socket for Postfix / Exim relay | ✅ |
| yarilo-auth | Passdb chain, auth cache, SASL dispatch, master userdb | ✅ |
| yarilo-warden | Connection rate limiting + penalty | ✅ |
| yarilo-locks | Cross-process write coordination — Redis-backed, TCP mTLS | ✅ |
| yarilo-quota-status | Quota policy socket (Postfix quota check) | ✅ |
| yarilo-fts | Full-text search indexer + lookup service (flatcurve/Xapian; sole cgo/libxapian process) | ✅ |
| yarilo-backend-api | HTTP admin API (dict, ACL, folder, quota, rebuild) | ✅ |
| yarilo-backend-reg | Co-located backend registration sidecar — one BACKEND-UP per pod IP, readiness-gated heartbeat, graceful LEAVE on SIGTERM (#776/#788) | ✅ |
| yarctl | CLI control tool — `director` and `backend` planes (backward-compat alias: `yarilo-admin`) | ✅ |
| yarilo-monitor | Optional backend health sidecar for the director ring (probe-based; the primary path is yarilo-backend-reg self-registration) | ✅ |
| yarilo-migrate | Offline mailbox FORMAT converter (Maildir → sdbox/mdbox); not cross-server dsync/imapc | ✅ |
| yarilo-director | Consistent-hashing ring, sticky sessions, throttled evacuation, failover | ✅ |

All intra-cluster protocols are TAB-delimited text with LF termination and a version handshake.

Session routing (`backend_addr` / `director_addr` precedence), sticky assignments, username-hash templates (`username_hash`), backend evacuation, the per-user flush hook, tag sharding models, and the self-organizing ring formation (with its design history) are all documented in **[DIRECTOR](https://doc.yarilomail.org/DIRECTOR)**.

---

## Quick start (Helm)

```sh
# Add the chart (local checkout)
helm upgrade --install yarilo ./helm \
  -f helm_values/values-sandbox.yaml \
  -n yarilo --create-namespace
```

See [the installation guide](https://doc.yarilomail.org/INSTALL) for a full Kubernetes walkthrough with cert-manager, Let's Encrypt, and an external MySQL passdb.

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
backends + auth/warden/locks + Redis) on one host, SQLite userdb, one image:

```sh
cd deploy/compose
cp .env.example .env
./gen-certs.sh mail.example.test     # self-signed TLS for local use
docker compose up -d
```

Full walkthrough — creating users, TLS, MTA (Postfix) integration, verifying,
backups — in [DOCKER-COMPOSE](https://doc.yarilomail.org/DOCKER-COMPOSE).

---

## Mailbox format migration (offline)

`yarilo-migrate` is an **offline, on-disk format converter** for a per-user mailbox tree — sources `maildir` / `dbox-v1` / `mdbox-v1`, destinations `sdbox` / `mdbox`. It is **not** a cross-server (dsync/imapc) migration that pulls mail over IMAP from another server.

```sh
yarilo-migrate \
  --from /var/mail/vhosts \
  --to   /var/mail/dbox \
  --format dbox   # or mdbox; --dry-run to preview
```

### GUID pre-migration

Mail stored before yarilo 2.3.8 carries no per-message GUID, so its `EMAILID`
(RFC 8474) is stamped the first time a client selects the folder. That one-off
pass is automatic and needs no operator action; this command only moves the cost
off the first `SELECT`, which is worth doing for very large folders.

```sh
yarilo-migrate --guid-backfill \
  --config /etc/yarilo/yarilo.yaml \   # layout, driver, userdb and yarilo-locks
  --user   u1@example.com               # optional: one user instead of all
                                        # --dry-run reports what is pending
```

| Flag | Meaning |
|:---|:---|
| `--guid-backfill` | Run the GUID pass instead of a format conversion |
| `--config` | `yarilo.yaml` supplying `storage.mailbox`, `storage.maildir_root`, `storage.mail_home_template`, `backend_api.auth_master_addr`, and the `yarilo-locks` client |
| `--driver` | Override `storage.mailbox`: `maildir` \| `sdbox` \| `mdbox` |
| `--root` | Override `storage.maildir_root` |
| `--home-template` | Override `storage.mail_home_template`, e.g. `%d/%u` |
| `--user` | Restrict to one `user@domain`; default is every user under the root |
| `--offline` | Resolve per-user paths from flags instead of userdb |
| `--index-template` | Offline stand-in for the userdb `INDEX=` override, e.g. `%h/index` |
| `--mail-template` | Offline stand-in for the userdb `mail_path` override |
| `--dry-run` | Report the folders that would be stamped, write nothing |

Per-user `INDEX=`, `CONTROL=`, `ALT=` and `mail_path` overrides live in the
userdb, not in `yarilo.yaml`, so by default the tool looks each user up through
`backend_api.auth_master_addr` exactly as a session does. A store whose auth is
not running is handled by `--offline` plus the templates; the two sources are
mutually exclusive, because a template disagreeing with userdb would address a
mailbox the sessions never use.

The templates take `~/` or `%h` for the user's home, plus `%u`/`%n`/`%d`, so a
userdb value of `INDEX=~/index` is written the same way here.

The tool never creates an index. A path holding no index is an error naming the
path, not an empty folder reported as complete.

Without `--config` both `--driver` and `--root` are required, and the run is
unlocked. Users are enumerated only for a layout whose leaf directory names the
user (`%u`, or `%n` with `%d` above it); any other `mail_home_template` has to be
driven one user at a time with `--user`.

The command writes to shared storage, so pass `--config` to make it take the
same locks the services take; it is then safe to run against a live store.
Without `--config` it runs unlocked, which is only safe with the store stopped.
Repeat runs are no-ops: a stamped folder is recorded as done and an already
assigned GUID is never rewritten.

---

## Documentation

Full documentation lives at **[doc.yarilomail.org](https://doc.yarilomail.org/)** (source: [yarilomail/documentation](https://github.com/yarilomail/documentation)).

| Document | Contents |
|:---|:---|
| [ARCHITECTURE](https://doc.yarilomail.org/ARCHITECTURE) | Code-level architecture, process model, storage contract, deployment diagrams |
| [DEPLOYMENT](https://doc.yarilomail.org/DEPLOYMENT) | K8s topology, sizing, HA strategy, sharding via tags |
| [GENERAL](https://doc.yarilomail.org/GENERAL) | `general`: SSL, HAProxy, XCLIENT, connection limits |
| [SERVICES](https://doc.yarilomail.org/SERVICES) | `services`: per-listener config |
| [IMAP](https://doc.yarilomail.org/IMAP) | `protocol.imap`: IDLE, line length, ACL, NAMESPACE, NOTIFY, METADATA, OBJECTID |
| [NAMESPACE](https://doc.yarilomail.org/NAMESPACE) | IMAP namespaces (RFC 2342 / 9051): personal / shared / other_users |
| [SUBMISSION](https://doc.yarilomail.org/SUBMISSION) | `protocol.submission`: hostname, size, relay |
| [LMTP](https://doc.yarilomail.org/LMTP) | `protocol.lmtp`: delivery, HAProxy, XCLIENT, TLS, headers |
| [POP3](https://doc.yarilomail.org/POP3) | `protocol.pop3`: UIDL, soft-delete, migration |
| [SIEVE](https://doc.yarilomail.org/SIEVE) | `sieve`: filtering, ManageSieve, imapsieve, vacation, extensions |
| [AUTH](https://doc.yarilomail.org/AUTH) | `auth.passdb`: SQL / passwd-file / static backends, password schemes, userdb extra fields |
| [QUOTA](https://doc.yarilomail.org/QUOTA) | `quota`: count-authoritative engine, grace, warnings, mail_size, clone mirror, over-status |
| [STORAGE](https://doc.yarilomail.org/STORAGE) | Mailbox self-healing, operator rebuild, mdbox rotation/tuning knobs |
| [FTS](https://doc.yarilomail.org/FTS) | Full-text search: engine, multi-language, decoders, config, phases |
| [SMOKE](https://doc.yarilomail.org/SMOKE) | End-to-end smoke test |
| [DIRECTOR](https://doc.yarilomail.org/DIRECTOR) | `director_service`: ring, peers, mTLS, session routing, sticky assignments, username-hash, evacuation, flush hook, ring formation history |
| [MONITOR](https://doc.yarilomail.org/MONITOR) | `yarilo-monitor`: health probes, Prometheus metrics |
| [DIRECTOR-API](https://doc.yarilomail.org/DIRECTOR-API) | Director HTTP admin API |
| [BACKEND-API](https://doc.yarilomail.org/BACKEND-API) | Backend HTTP admin API |
| [YARILO-ADMIN](https://doc.yarilomail.org/YARILO-ADMIN) | `yarctl` CLI reference |
| [DICT](https://doc.yarilomail.org/DICT) | `pkg/dict` KV-store abstraction: drivers, YAML schema |

---

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).
