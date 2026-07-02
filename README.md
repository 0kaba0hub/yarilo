# yarilo

<table><tr>
<td><img src="https://raw.githubusercontent.com/0kaba0hub/yarilo/main/docs/icon.svg" width="180" alt="yarilo logo"/></td>
<td>

Production-grade IMAP / POP3 / LMTP / ManageSieve / Submission mail server written in Go.
Multi-binary architecture — each protocol component is a separate process. Full Dovecot 2.3 protocol compatibility. Kubernetes-native via Helm.

[![CI](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml/badge.svg)](https://github.com/0kaba0hub/yarilo/actions/workflows/ci.yml)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-blue)](https://github.com/0kaba0hub/yarilo)
[![License: GPL v3](https://img.shields.io/badge/license-GPLv3-blue.svg)](LICENSE)
[![Status: pre-alpha](https://img.shields.io/badge/status-pre--alpha-orange)](PLAN.md)

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

**yarilo-auth** — shared passdb chain (MySQL / Postgres / SQLite / LDAP), auth-token cache, SASL dispatch, master protocol for userdb lookups.

**yarilo-locks** — cross-process write coordination. TCP mTLS `:9104`, Redis-backed state. All Kubernetes deployments (standalone and backend) use remote mode — embedded Unix-socket mode is reserved for unit tests and single-process CLI runs.

**yarilo-anvil** — connection rate limiting and penalty tracking. Shared across all login pods.

---

## Protocol support

| Protocol | Standard | Extensions | Status |
|:---|:---|:---|:---|
| IMAP4rev2 | RFC 9051 | IDLE, MOVE, CONDSTORE, QRESYNC, UNSELECT, NAMESPACE, QUOTA, ACL, BINARY, UIDPLUS, SORT, THREAD, ESEARCH, NOTIFY, URLAUTH, SPECIAL-USE | ✅ |
| POP3 | RFC 1939 | STLS, UIDL, CAPA, XCLIENT | ✅ |
| LMTP | RFC 2033 | per-recipient status, HAProxy, XCLIENT, STARTTLS, `Delivered-To`, Sieve delivery | ✅ |
| ManageSieve | RFC 5804 | full script management | ✅ |
| Sieve | RFC 5228 | fileinto, reject, ereject, envelope, encoded-character, copy, subaddress, variables, relational, body, vacation, vacation-seconds, regex, date, index, editheader, mailbox, duplicate, ihave, special-use, imap4flags, fcc, include, extlists, enotify, environment, spamtest, spamtestplus, virustest, mboxmetadata, servermetadata | ✅ |
| Submission | RFC 6409 | STARTTLS, SASL PLAIN, SIZE, PIPELINING, relay to upstream MTA | ✅ |
| SASL | — | PLAIN, LOGIN, SCRAM-SHA-256, XOAUTH2, OAUTHBEARER, GSSAPI | ✅ |
| JMAP | RFC 8620/8621 | — | planned |

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
| yarilo-backend-api | HTTP admin API (dict, ACL, folder, quota, rebuild) | ✅ |
| yarilo-admin | CLI control tool — `director` and `backend` planes | ✅ |
| yarilo-monitor | Backend health sidecar for director ring | ✅ |
| yarilo-migrate | Offline mailbox migration (Maildir ↔ dbox ↔ mdbox) | ✅ |
| yarilo-director | Consistent-hashing ring, sticky sessions, failover | planned |

All intra-cluster protocols are TAB-delimited text with LF termination and a version handshake.
Full wire-format specification: [INTERNALS.md](INTERNALS.md).

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
| [docs/IMAP.md](docs/IMAP.md) | `protocol.imap`: IDLE, line length, ACL, NAMESPACE |
| [docs/NAMESPACE.md](docs/NAMESPACE.md) | IMAP namespaces (RFC 2342 / 9051): personal / shared / other_users |
| [docs/SUBMISSION.md](docs/SUBMISSION.md) | `protocol.submission`: hostname, size, relay |
| [docs/LMTP.md](docs/LMTP.md) | `protocol.lmtp`: delivery, HAProxy, XCLIENT, TLS, headers |
| [docs/POP3.md](docs/POP3.md) | `protocol.pop3`: UIDL, soft-delete, migration |
| [docs/AUTH.md](docs/AUTH.md) | `auth.passdb`: SQL backends, password schemes |
| [docs/SMOKE.md](docs/SMOKE.md) | End-to-end smoke test |
| [docs/DIRECTOR.md](docs/DIRECTOR.md) | `director_service`: ring, peers, HAProxy, XCLIENT, mTLS |
| [docs/MONITOR.md](docs/MONITOR.md) | `yarilo-monitor`: health probes, Prometheus metrics |
| [docs/DIRECTOR-API.md](docs/DIRECTOR-API.md) | Director HTTP admin API |
| [docs/BACKEND-API.md](docs/BACKEND-API.md) | Backend HTTP admin API |
| [docs/YARILO-ADMIN.md](docs/YARILO-ADMIN.md) | `yarilo-admin` CLI reference |
| [docs/DICT.md](docs/DICT.md) | `pkg/dict` KV-store abstraction: drivers, YAML schema |

---

## License

GNU General Public License v3.0 — see [LICENSE](LICENSE).
