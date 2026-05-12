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
helm/yarilo/     — Helm chart (Chart.yaml, values.yaml, templates/)
docker/          — Dockerfile
```

---

## Architecture rules

- **Single binary, three roles**: `mode: proxy | director | backend | single`.
  Never create separate binaries per role.
- **`pkg/mailbox` interfaces are the contract** between all storage implementations.
  Never import `internal/storage/*` from outside `internal/backend`.
- **Each worker writes only to its own tables/files.**
  No cross-package writes. Concurrent writes = corruption.
- **API reads DB, workers write DB.** (applies when DB layer is added)
- **Proxy is stateless.** No session state on the proxy — it routes and splices TCP.
- **Director owns the ring.** Nothing else modifies backend assignment.
- **Internal protocols** (director, auth, dict) are TAB-delimited, LF-terminated,
  with version handshake. See INTERNALS.md for exact wire format.

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
- `release`: triggered automatically when `helm/yarilo/Chart.yaml` `appVersion` is a new tag —
  git-cliff generates release notes, `gh release create` publishes.

Never push directly to `main`. Feature branch → PR → user merges.

---

## Helm chart

- Chart lives in `helm/yarilo/`. One chart, one app.
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
- dovecot-uidlist version 3: header `3 V<uidvalidity> N<nextuid> G<guid128hex>`.

---

## Release checklist

Before bumping `appVersion` in `helm/yarilo/Chart.yaml`:
1. All unit tests pass (`go test ./...`)
2. `helm lint helm/yarilo` passes
3. Smoke test passes against staging
4. README updated if new feature/env var/value was added
