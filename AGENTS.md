# yarilo — repository conventions

Extends the workspace-level `/CLAUDE.md`. All global rules apply.
Rules here are **yarilo-specific** and take precedence where they overlap.

---

## Repository layout

```
app/
  yarilo/        — main binary (entry point only, no business logic)
  smoketest/     — post-deploy smoke test binary
internal/
  auth/          — passdb chain, yarilo-auth UNIX socket protocol
  backend/       — single-node wiring of all components
  cluster/
    ring/        — consistent hashing ring (MD5, 100 vhosts)
    proto/       — yarilo-director TAB-delimited wire protocol
  imap/          — IMAP server (go-imap/v2 + extensions)
  storage/
    mailbox/     — MailboxBackend implementations (maildir, dbox, …)
    index/       — IndexBackend implementations (fileindex, sqlite, …)
  telemetry/     — /healthz, /readyz, /metrics
pkg/
  mailbox/       — MailboxBackend + IndexBackend interfaces, core types
  config/        — YAML config via koanf
helm/     — Helm chart (Chart.yaml, values.yaml, templates/)
docker/          — Dockerfile
```

---

## Infrastructure architecture — the core concept

**yarilo's infrastructure architecture is defined by these documents and diagrams in `docs/`. They are the source of truth for every decision about deployment, scaling, HA, and cross-component coordination:**

- **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** — deployment topology, sizing (per pod, per tag), HA strategy, sharding via tags, and the rationale behind each decision
- **[docs/yarilo_director.svg](docs/yarilo_director.svg)** — director deployment: login proxies, a 3-pod director StatefulSet with peer-sync, backend-lease (self-registration + heartbeat #776, replacing the monitor sidecars), ring routing to backend tags
- **[docs/yarilo_backend.svg](docs/yarilo_backend.svg)** — backend deployment (per tag): ONE co-located StatefulSet whose pod carries every protocol container (imap/pop3/submission/lmtp/managesieve) plus `yarilo-fts` and the `yarilo-backend-reg` sidecar on a shared IP; `yarilo-locks`, `backend-api` and `quota-status` are separate deployments; one shared NFS PV (RWX)
- **[docs/yarilo_standalone.svg](docs/yarilo_standalone.svg)** — standalone deployment: the full stack (login + sessions + auth + warden + embedded `yarilo-locks` + storage) for self-contained installations without a director

**How to use them:**
1. Any change to the infrastructure approach (new pods, a different scaling model, new services, changed coordination) **starts by updating these diagrams and DEPLOYMENT.md**, not by writing code.
2. When code contradicts a diagram, either the code is corrected to match or the diagram is updated with a stated rationale.
3. When planning a feature with infrastructure consequences, check the diagrams first, discuss the change, then update the document.

**Key architectural decisions, as recorded in the diagrams:**
- ONE co-located StatefulSet per backend deployment — the pod carries every protocol container (imap/pop3/submission/lmtp/managesieve) plus `yarilo-fts` (fts_addr=localhost) and the `yarilo-backend-reg` sidecar on a shared IP. This restores the reference invariant that one mail host owns all of a user's per-user resources, which is what makes a single ring and a single userDir correct (#788); co-locating fts gives a single writer per user index for free (#675/#676). Independent per-protocol scaling is given up deliberately in exchange for routing coherence — consistent hashing cannot hold "one user, one pod" across separate pools
- `yarilo-auth` and `yarilo-warden` are shared services (two Deployments), one deployment per installation
- `yarilo-locks` — single abstraction for cross-process write coordination. **All k8s deployments (standalone and backend) use `remote` mode** — its own Deployment behind a ClusterIP Service, mTLS TCP `:9104`, Redis-backed state. `embedded` mode (in-memory + Unix socket) is reserved for unit tests and non-k8s CLI runs; it is never the production default because Unix sockets cannot cross pods, which breaks any `replicaCount > 1`. In-process goroutine concurrency stays on `sync.Mutex` as a two-tier fast-path.
- One NFS PV (RWX) per tag, shared by every co-located pod within that tag; a `tag` is an NFS shard, NOT a protocol
- Director is a 3-replica StatefulSet with peer-sync, ONE ring and one userDir — a single pod IP per user serves every protocol, and the login proxy overrides the port
- Sticky routing is per user, not per protocol: a user is pinned to one co-located pod for every protocol, and cross-pod coordination goes through `yarilo-locks`
- TLS termination and passdb happen at the director; userdb happens at the backend via the shared `yarilo-auth`

---

## Architecture rules

**ARCHITECTURE.md is the single source of truth for code-level architecture.** Read it before any implementation work.
Every decision about processes, UIDs, IPC, storage is defined there.
Code that contradicts ARCHITECTURE.md is wrong — fix the code, not the document.

**For the infrastructure and deployment layer, see `docs/DEPLOYMENT.md` and the SVG diagrams above.**

Key rules derived from ARCHITECTURE.md:

- **Multi-binary, multi-process.** Each component is a separate compiled binary in `app/`.
  Never create a single binary with mode flags. Never put two components in one `main.go`.
- **Config-not-binary.** Every deployment / scaling / topology change goes through configuration
  only (`yarilo.yaml` or Helm `values.yaml`). The compiled binaries do not change between shapes:
  one `yarilo-locks` artefact serves embedded mode (tests / dev CLI) and remote mode (k8s prod);
  one `yarilo-imap` (and each session binary) serves single-node standalone and sharded backend.
  No build tags (`//go:build standalone`), no compile-time switches, no runtime `if isStandalone()`
  branches in code that gate anything beyond reading a config value. Scaling `replicaCount: 1 → N`,
  switching bundled Redis ↔ external Redis, migrating standalone → backend with director — all
  values.yaml changes, never code changes. Backends behind interfaces are fine; code-level
  branching on deployment shape is not.
- **`exec.Command` only — never `fork()`.** Go runtime + fork = undefined behavior.
- **Privilege drop via `SysProcAttr.Credential` only.** `syscall.Setuid()` does not work
  in multi-threaded Go. Credential is set at process start, before Go runtime initializes.
- **Login process is TLS terminator for session lifetime.** `yarilo-imap-login` holds the
  TLS conn and proxies plain bytes to `yarilo-imap` via Unix socket pair.
  Session processes have zero TLS knowledge.
- **fd-passing via SCM_RIGHTS.** Master binds ports, passes listening fd to login processes.
  Login passes authenticated conn fd to master after auth success.
- **`pkg/mailbox` interfaces are the contract** between all storage implementations.
  Never import `internal/storage/*` from outside the session process packages.
- **Per-user storage handle (Dovecot `mail_storage` pattern).**
  Storage backends expose `OpenUser(*UserInfo) UserMailbox`. Sessions call `OpenUser` once
  after auth. Handle methods take NO user/path parameter — `UserInfo` is captured at Open time.
- **Director owns the ring.** Nothing else modifies backend assignment.
- **Internal protocols** (director, auth, warden, ipc) are TAB-delimited, LF-terminated,
  with version handshake. See docs_internal/INTERNALS.md for exact wire format.
- **Each process writes only to its own resources.** No cross-process writes to shared state.
- **Cross-process write coordination always goes through `yarilo-locks`.** Single `pkg/locks` API,
  single TAB-delimited wire protocol. **In every k8s Helm release (standalone or backend) the
  binary runs in `remote` mode** — its own Deployment, mTLS TCP `:9104`, Redis-backed state.
  `embedded` mode (Unix socket + in-memory) exists for unit tests and CLI dev runs only;
  it cannot scale across pods so it is never the k8s default. Never use `fcntl`/`flock`, never
  bypass with raw `sync.Mutex` across process boundaries. `sync.Mutex` stays only as in-process
  fast-path before any `yarilo-locks` call.
- **Dovecot feature parity is the North Star.** Every feature that exists in Dovecot is in-scope
  for yarilo over time — namespaces (personal/shared/public), vdirs (virtual mailboxes), Sieve,
  ManageSieve, fts (full-text search), replication (dsync), quota, ACL, imap-hibernate,
  password schemes, etc. "Not in this PR" and "next phase" are acceptable; "out of scope forever"
  is not. Audit gaps against Dovecot when designing each phase.
- **Every Dovecot-modelled feature ships with `yarilo.yaml` + Helm `values.yaml` knobs.** Tunables
  are exposed in `pkg/config/config.go` under the relevant section (`protocol.*` / `auth.*` /
  `storage.*` / `general.*`) AND in `helm/values.yaml`. Defaults may match Dovecot's defaults
  but operators must always be able to override. No hard-coded behaviour that is not also a
  config knob. Match Dovecot's option names where reasonable so `dovecot.conf` → `yarilo.yaml`
  migration stays mechanical.

---

## Go code style

- **No comments** unless the WHY is non-obvious (hidden constraint, subtle invariant,
  Dovecot-compatibility quirk). Never explain WHAT the code does.
- **No half-finished implementations.** Stub with `return errors.New("not yet implemented")`
  — never leave a function body empty or silently broken.
- **Error wrapping**: always `fmt.Errorf("package/op: %w", err)` — never bare `err`.
- **Table-driven tests**: all test cases in `[]struct{ ... }` slices. No ad-hoc repeated calls.
- **No `init()` functions.** Wire everything explicitly in `backend.New()` or `main()`.
- **Context propagation**: every long-running operation accepts `context.Context` as first arg.
- **`t.TempDir()`** for all test filesystem work — never hardcoded `/tmp` paths.

---

## Logging

- `log/slog` with JSON handler everywhere. Never `log.Printf`, `log.Fatal`, `fmt.Println`.
- `LOG_LEVEL=debug` enables `slog.LevelDebug` at startup — no code changes needed.
- Every significant operation logs at `slog.Info` with structured key-value pairs.
- Errors always logged with `slog.Error("context msg", "err", err)`.

---

## Testing

### Unit tests
- **Every new package gets `*_test.go` in the same commit** that introduces the package.
- Cover: happy path, edge cases (empty input, not-found, concurrent access where relevant).
- Use `t.TempDir()` for filesystem tests — never leave temp files behind.
- Forbidden in unit tests: network connections, real IMAP/SMTP ports, sleep.

### Post-deploy smoke tests (`app/smoketest`)
- **Every new protocol port gets a smoke check** added to `app/smoketest/main.go`.
- Smoke test connects to the live deployment: IMAP TLS greeting, `/healthz`, `/readyz`.
- Run via `smoke.yml` workflow (manual dispatch or called from deploy pipeline).
- Exit 0 = all checks passed. Exit 1 = failure with per-check output.

### Integration tests (future)
- Will live in `test/integration/` — not yet implemented.

---

## CI pipeline (`ci.yml`)

Order: **lint → test → build & push → release**.

- `lint`: golangci-lint + hadolint + `helm lint`
- `test`: `go test ./...` with go-junit-report → dorny/test-reporter
- `build`: GHCR push on `main` only; tag = git SHA + `latest` + semver if new release
- `release`: triggered automatically when `helm/Chart.yaml` `appVersion` is a new tag —
  git-cliff generates release notes, `gh release create` publishes.

Never push directly to `main`. Feature branch → PR → user merges.

---

## Helm chart

- Chart lives in `helm/`. One chart, one app.
- `appVersion` in `Chart.yaml` is the **single source of truth** for the release version.
  Bump it → CI creates the GitHub Release and GHCR tag automatically.
- `strategy: Recreate` — ensures old pod releases the PVC before new pod mounts it.
- `replicaCount: 1` for single-node mode. Multi-node uses yarilo-director (Phase 5).

---

## Dovecot compatibility

- Wire formats for all internal protocols are documented in `docs_internal/INTERNALS.md`.
  **Always consult docs_internal/INTERNALS.md before implementing any binary format or internal socket.**
- Magic bytes, version numbers, field offsets — must match exactly.
  See §7 (FileIndex), §8 (Maildir), §2 (director protocol), §3 (auth protocol).
- Maildir filenames: `{secs}.M{usecs}P{pid}_{seq}.{hostname}:2,{flags}` — flags sorted uppercase.
- yarilo-uidlist version 3: header `3 V<uidvalidity> N<nextuid> G<guid128hex>`.

---

## Release checklist

Before bumping `appVersion` in `helm/Chart.yaml`:
1. All unit tests pass (`go test ./...`)
2. `helm lint helm/yarilo` passes
3. Smoke test passes against staging
4. README updated if new feature/env var/value was added
