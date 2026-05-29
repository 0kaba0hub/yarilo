# yarilo — project rules for Claude

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

## Infrastructure architecture — головна концепція

**Інфраструктурна архітектура yarilo визначена цими документами/схемами в `docs/`. Це source of truth для будь-яких рішень по deployment, scaling, HA, координації між компонентами:**

- **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** — топологія deployment, sizing (per pod, per tag), HA strategy, sharding через tags, обґрунтування рішень
- **[docs/yarilo_director.svg](docs/yarilo_director.svg)** — director deployment: login-proxies, 3-pod director StatefulSet з peer-sync, monitor sidecars, ring-routing до backend tags
- **[docs/yarilo_backend.svg](docs/yarilo_backend.svg)** — backend deployment (per tag): 4 окремі StatefulSet-и на протокол (imap/pop3/submission/lmtp) для незалежного scaling, yarilo-locks для cross-pod coordination, shared NFS PV (RWX)
- **[docs/yarilo_standalone.svg](docs/yarilo_standalone.svg)** — standalone deployment: повний стек (login + sessions + auth + anvil + `yarilo-locks` embedded + storage) для self-contained інсталяцій без director-а

**Правила використання:**
1. Будь-яка зміна в infrastructure approach (нові поди, зміна scaling-моделі, нові services, зміна координації) **починається з оновлення цих схем + DEPLOYMENT.md**, а не з коду.
2. Якщо код суперечить схемі — або код виправляється під схему, або схема оновлюється з обґрунтуванням.
3. При планувані нової функціональності з infrastructure-наслідками — перевір схеми, обговори зміни, оновлюй документ.

**Ключові архітектурні рішення зафіксовані в схемах:**
- 4 окремих StatefulSet-и на протокол у backend deployment — для independent scaling per protocol
- `yarilo-auth` + `yarilo-anvil` — shared services (Deployments × 2), один deployment на всю інсталяцію
- `yarilo-locks` — single abstraction for cross-process write coordination. **All k8s deployments (standalone and backend) use `remote` mode** — its own Deployment behind a ClusterIP Service, mTLS TCP `:9104`, Redis-backed state. `embedded` mode (in-memory + Unix socket) is reserved for unit tests and non-k8s CLI runs; it is never the production default because Unix sockets cannot cross pods, which breaks any `replicaCount > 1`. In-process goroutine concurrency stays on `sync.Mutex` as a two-tier fast-path.
- Один NFS PV (RWX) на tag — shared всіма 4 StatefulSet-ами в межах tag-у
- Director — StatefulSet × 3 з peer-sync, 4 окремі ring-и (по одному на протокол)
- Sticky routing per-protocol, cross-protocol coordination через `yarilo-locks`
- TLS terminate + passdb на director, userdb на backend через shared `yarilo-auth`

---

## Architecture rules

**ARCHITECTURE.md is the single source of truth for code-level architecture.** Read it before any implementation work.
Every decision about processes, UIDs, IPC, storage is defined there.
Code that contradicts ARCHITECTURE.md is wrong — fix the code, not the document.

**Для infrastructure/deployment рівня — see `docs/DEPLOYMENT.md` + SVG-схеми вище.**

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
- **Internal protocols** (director, auth, anvil, ipc) are TAB-delimited, LF-terminated,
  with version handshake. See INTERNALS.md for exact wire format.
- **Each process writes only to its own resources.** No cross-process writes to shared state.
- **Cross-process write coordination always goes through `yarilo-locks`.** Single `pkg/locks` API,
  single TAB-delimited wire protocol. **In every k8s Helm release (standalone or backend) the
  binary runs in `remote` mode** — its own Deployment, mTLS TCP `:9104`, Redis-backed state.
  `embedded` mode (Unix socket + in-memory) exists for unit tests and CLI dev runs only;
  it cannot scale across pods so it is never the k8s default. Never use `fcntl`/`flock`, never
  bypass with raw `sync.Mutex` across process boundaries. `sync.Mutex` stays only as in-process
  fast-path before any `yarilo-locks` call.

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

- Wire formats for all internal protocols are documented in `INTERNALS.md`.
  **Always consult INTERNALS.md before implementing any binary format or internal socket.**
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
