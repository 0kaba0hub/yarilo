# yarilo

<table><tr>
<td><img src="https://raw.githubusercontent.com/0kaba0hub/yarilo/main/docs/icon.svg" width="180" alt="yarilo logo"/></td>
<td>

Production-grade IMAP / POP3 / LMTP / ManageSieve / Submission mail server with built-in full-text search, written in Go.
Multi-binary architecture — each protocol component is a separate process. Kubernetes-native via Helm.

[![CI](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml/badge.svg)](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-blue)](https://github.com/0kaba0hub/yarilo)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPLv3-blue.svg)](LICENSE)
![Status: beta](https://img.shields.io/badge/status-beta-orange)

</td>
</tr></table>

---

## Architecture

Yarilo is a **multi-binary** server. Each protocol and infrastructure role is a separate compiled binary — no mode flags, no combined processes. Deployment topology is configured purely through Helm values; the same binaries serve standalone and clustered installations.

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

Self-healing (Maildir sync-on-open, dbox/mdbox reactive heal), the operator rebuild path, and mdbox rotation/tuning knobs: see **[docs/STORAGE.md](docs/STORAGE.md)**.

---

## Full-text search

`SEARCH BODY`, `SEARCH TEXT` and `SEARCH HEADER` are backed by a per-user full-text index instead of a linear message scan. `SEARCH RETURN (RELEVANCY)` (RFC 4731/6203) surfaces the engine's ranking as scores 1-100, min-max normalized per result set — requires a [`yarilo-patches`](https://github.com/0kaba0hub/go-imap/tree/yarilo-patches) go-imap fork, since upstream has no RELEVANCY support.

| Component | Backend | Status |
|:---|:---|:---|
| Engine | flatcurve (Xapian, on-disk glass shards) via [`go-xapian`](https://github.com/0kaba0hub/go-xapian) | ✅ |
| Indexer / lookup service | `yarilo-fts` (sole writer; sessions dial it) | ✅ |

The `yarilo-fts` service owns the index end-to-end (indexing *and* lookups) and is the only process linking libxapian (cgo); session binaries stay pure-Go and send `LOOKUP` over the internal TAB protocol. Enable it with `fts.enabled` + `fts_engine: flatcurve`. Multi-language indexing/search, attachment decoders, and the full config surface are documented in **[docs/FTS.md](docs/FTS.md)**.

**Acceptance benchmark** (`app/fts-bench`, synthetic corpus, local Xapian glass shards):

| Corpus | Shards | Index size | Index rate | SEARCH p95 (indexed vs scan) |
|:---|:---|:---|:---|:---|
| 5,000 | 1 | 1.59× corpus | 9,654 msg/s | 0.08 ms vs 77.7 ms (**942×**) |
| 10,000 | 2 | 1.62× corpus | 9,764 msg/s | 0.14 ms vs 149 ms (**1,090×**) |
| 20,000 | 4 | 1.63× corpus | 10,027 msg/s | 0.23 ms vs 322 ms (**1,410×**) |

Search stays sub-millisecond as the mailbox grows; the linear scan it replaces grows with message count. See `docs/FTS.md` for the full design and the phased roadmap (relevancy / strict-substring / multi-language, then attachment decoders).

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
| yarilo-backend-reg | Co-located backend registration sidecar — one BACKEND-UP per pod IP, readiness-gated heartbeat, graceful LEAVE on SIGTERM (#776/#788) | ✅ |
| yarctl | CLI control tool — `director` and `backend` planes (backward-compat alias: `yarilo-admin`) | ✅ |
| yarilo-monitor | Optional backend health sidecar for the director ring (probe-based; the primary path is yarilo-backend-reg self-registration) | ✅ |
| yarilo-migrate | Offline mailbox FORMAT converter (Maildir → sdbox/mdbox); not cross-server dsync/imapc | ✅ |
| yarilo-director | Consistent-hashing ring, sticky sessions, throttled evacuation, failover | ✅ |

All intra-cluster protocols are TAB-delimited text with LF termination and a version handshake.

Session routing (`backend_addr` / `director_addr` precedence), sticky assignments, username-hash templates (`username_hash`), backend evacuation, the per-user flush hook, tag sharding models, and the self-organizing ring formation (with its design history) are all documented in **[docs/DIRECTOR.md](docs/DIRECTOR.md)**.

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

## Mailbox format migration (offline)

`yarilo-migrate` is an **offline, on-disk format converter** for a per-user mailbox tree — sources `maildir` / `dbox-v1` / `mdbox-v1`, destinations `sdbox` / `mdbox`. It is **not** a cross-server (dsync/imapc) migration that pulls mail over IMAP from another server.

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
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Code-level architecture, process model, storage contract, deployment diagrams |
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
| [docs/STORAGE.md](docs/STORAGE.md) | Mailbox self-healing, operator rebuild, mdbox rotation/tuning knobs |
| [docs/FTS.md](docs/FTS.md) | Full-text search: engine, multi-language, decoders, config, phases |
| [docs/SMOKE.md](docs/SMOKE.md) | End-to-end smoke test |
| [docs/DIRECTOR.md](docs/DIRECTOR.md) | `director_service`: ring, peers, mTLS, session routing, sticky assignments, username-hash, evacuation, flush hook, ring formation history |
| [docs/MONITOR.md](docs/MONITOR.md) | `yarilo-monitor`: health probes, Prometheus metrics |
| [docs/DIRECTOR-API.md](docs/DIRECTOR-API.md) | Director HTTP admin API |
| [docs/BACKEND-API.md](docs/BACKEND-API.md) | Backend HTTP admin API |
| [docs/YARILO-ADMIN.md](docs/YARILO-ADMIN.md) | `yarctl` CLI reference |
| [docs/DICT.md](docs/DICT.md) | `pkg/dict` KV-store abstraction: drivers, YAML schema |
| [docs/TODO.md](docs/TODO.md) | Deferred work / backlog (AUTH mechs & drivers, obox, dsync, FTS engines) |

---

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).
