# yarilo — standalone deployment implementation plan

End-to-end plan to ship a working standalone deployment of yarilo (single pod,
all components co-located, no director, no Redis dependency).

Dependencies inside a phase are sequential; phases that can run in parallel
are marked in the Dependency graph at the end.

Each new package gets `*_test.go` in the same commit (per CLAUDE.md).
After every phase: `golangci-lint`, `go test ./...`, and (from Phase 6 onward)
`helm lint` must be green.

---

## Phase 0 — `pkg/locks` foundation

- [ ] Design the `pkg/locks` API: `Locker` interface (`Lock(ctx, resource, owner, ttl) (id, error)`, `Unlock(id)`, `Renew(id, ttl)`, `Subscribe(resource) <-chan Event`), error sentinels (`ErrBusy`, `ErrExpired`).
- [ ] Implement `locks.Embedded` — in-memory `map[string]lockState` with a background TTL sweeper, Unix-socket listener (TAB-delimited wire protocol), no TLS.
- [ ] Implement `locks.Remote` — TCP+mTLS client backed by Redis (Lua `SET ... NX EX`).
- [ ] `yarilo-locks` binary with `--embedded` / `--remote` flags, configured from `yarilo.yaml`.
- [ ] Single integration-test suite drives the same wire protocol against both backends via `pkg/locks.Locker`.
- [ ] Metrics: `yarilo_locks_acquire_seconds`, `yarilo_locks_busy_total`, `yarilo_locks_renew_failed_total`. Logs via `slog.With("locks_mode", ...)`.
- [ ] `/healthz` and `/readyz` endpoints.

## Phase 1 — Wire locks into storage write paths

Resolves the `ARCHITECTURE.md` §Known issues item.

- [ ] Audit `internal/storage/mailbox/maildir` and `internal/storage/index/file`: list every write point (`yarilo-uidlist`, fileindex `*.idx`, `.index.log`, `.index.cache`).
- [ ] Two-tier wrapper: in-process `sync.Mutex` fast-path → `pkg/locks` cross-process barrier before any shared-file write.
- [ ] Migrate UID assignment (atomic read-increment-write of `NEXTUID`) under the X lock.
- [ ] Migrate APPEND / EXPUNGE / STORE / RENAME / DELETE under the X lock on `mbox:<user>:<folder>`.
- [ ] LMTP delivery under the X lock on the same resource keys, respecting the convention order `idx` → `mbox` → `deliver`.
- [ ] Integration test: two processes × N goroutines concurrently write the same mailbox via embedded locks — assert no duplicate UIDs and no index corruption.
- [ ] Benchmark: embedded-lock overhead vs raw `sync.Mutex` (target: < 500 µs RTT per LOCK/UNLOCK pair).

## Phase 2 — Session processes (functional completeness)

Depends on Phase 1 (storage under locks).

### `yarilo-imap`
- [ ] `AUTHENTICATE` + state machine (IMAP4rev2 base: SELECT, EXAMINE, CREATE, DELETE, RENAME, LIST, LSUB, FETCH, STORE, APPEND, EXPUNGE, COPY, MOVE, UID variants, IDLE, NAMESPACE, CAPABILITY, ID, NOOP, LOGOUT, UNSELECT, CHECK, CLOSE).
- [ ] Extensions: UIDPLUS, MOVE, CONDSTORE, ESEARCH, SORT, THREAD, BINARY, ENABLE, UTF8=ACCEPT (via `go-imap/v2` where possible).
- [ ] IDLE notifications via the `pkg/locks` EVENT channel — pushed when APPEND/EXPUNGE/STORE happens in another process.
- [ ] Storage handle `OpenUser(*UserInfo)` after preamble-auth.

### `yarilo-pop3`
- [ ] USER/PASS, STAT, LIST, RETR, DELE, NOOP, RSET, QUIT, CAPA, UIDL, TOP, STLS.
- [ ] UIDL format compatibility with Dovecot (see the relevant `INTERNALS.md` section).
- [ ] Soft-delete until QUIT, hard-delete at QUIT (under the X lock).

### `yarilo-submission`
- [ ] EHLO, MAIL FROM, RCPT TO, DATA, RSET, NOOP, QUIT, STARTTLS, AUTH PLAIN, SIZE, PIPELINING, 8BITMIME.
- [ ] Outbound relay to upstream MTA (config: `submission.relay_host`).
- [ ] DKIM signing on outbound (config: `submission.dkim_keys`).
- [ ] Sieve hook (basic: outbound script execution to choose Sent-folder placement).

### `yarilo-lmtp`
- [ ] LHLO, MAIL FROM, RCPT TO (per-recipient status), DATA, RSET, NOOP, QUIT.
- [ ] Per-recipient fan-out (already merged via PRs #80–#82) — confirm it works with standalone locks.
- [ ] Delivered-To header, Received header, Sieve filtering hook on delivery.
- [ ] Sieve `fileinto` → X lock on target mailbox.

Cross-cutting for all four: `pkg/mailbox` interface contract, `/healthz`, `/readyz`, `/metrics`, `LOG_LEVEL=debug`, graceful shutdown via `sessionGracePeriod` / `killTimeout`.

## Phase 3 — Login proxies (TLS terminator + fd-pass)

Depends on Phase 2 (need session processes to proxy to).

- [ ] `yarilo-imap-login`: TLS accept (SNI cert reload), SASL chain (PLAIN, LOGIN, OAUTHBEARER), passdb call to `yarilo-auth`, preamble write (auth state, rip/lip, session ID), SCM_RIGHTS fd-pass to `yarilo-imap` over Unix socket.
- [ ] `yarilo-pop3-login` — same pattern, POP3 wire protocol (USER/PASS, APOP, STLS).
- [ ] `yarilo-submission-login` — same pattern, SMTP submission (EHLO/STARTTLS/AUTH).
- [ ] `yarilo-lmtp-login` (MTA-facing) — typically no TLS (private network), optional STARTTLS. Inbound: HAProxy PROXY v1/v2, XCLIENT with `DESTADDR`/`DESTPORT`.
- [ ] Trusted-nets enforcement: `xclient_trusted_nets`, `haproxy.trusted_nets` per protocol instance.
- [ ] Login → anvil CONNECT/DISCONNECT events, pre-admit check against `mail_max_userip_connections`.
- [ ] Session-ID generation in the login process: `base64(microseconds[48]|remote_port[16]|remote_ip_bytes)` (per `ARCHITECTURE.md` §Session ID).
- [ ] `/healthz`, `/readyz`, `/metrics`.

## Phase 4 — Shared services (parallel with Phases 2/3)

### `yarilo-auth`
- [ ] passdb chain: SQLite, PostgreSQL, MySQL drivers (per `docs/AUTH.md`).
- [ ] userdb chain: same drivers.
- [ ] SASL dispatch: PLAIN, LOGIN, OAUTHBEARER (XOAUTH2 where feasible).
- [ ] Auth cache (TTL-based, in-memory) to reduce passdb load.
- [ ] Password schemes: SHA512-CRYPT, BLF-CRYPT, ARGON2ID (Dovecot compatibility).
- [ ] mTLS TCP `:9100` listener (per `ARCHITECTURE.md`).
- [ ] `/healthz`, `/readyz`, `/metrics`.

### `yarilo-anvil`
- [ ] CONNECT/DISCONNECT event store (per-IP counters with TTL).
- [ ] SESSION_START / SESSION_END store (per-user counters).
- [ ] Query API for login proxies: "can this IP/user open another connection?".
- [ ] Backend: Redis (for backend deployments) plus an in-memory option (for self-contained standalone).
- [ ] mTLS TCP `:9101`.
- [ ] `/healthz`, `/readyz`, `/metrics`.

## Phase 5 — Configuration, lifecycle, observability

- [ ] `pkg/config`: standalone preset (single YAML file with every component as a section), schema validation via koanf.
- [ ] Graceful shutdown wired per `ARCHITECTURE.md` §Graceful shutdown: SIGTERM → stop accept → drain up to `sessionGracePeriod` → close → SIGKILL after `killTimeout`.
- [ ] `LOG_LEVEL=debug` → `slog.LevelDebug` at startup for every binary (no code changes required).
- [ ] Structured slog fields per `ARCHITECTURE.md` §slog field names (`process`, `pid`, `user`, `session`, `rip`, `rport`, `lip`, `lport`, `tls`, etc.).
- [ ] Prometheus metrics per binary; ServiceMonitor manifest in the Helm chart.
- [ ] TLS cert paths configurable; SNI per-domain (full feature lands later, but config schema lands here).

## Phase 6 — Helm chart + Docker for standalone

- [ ] `helm/yarilo-standalone` chart: one Pod with multiple containers (login × 4, session × 4, auth, anvil, locks-embedded) sharing `/run/yarilo/*.sock` via an `emptyDir` volume.
- [ ] ConfigMap for `yarilo.yaml`, Secret for TLS cert/key and auth DB credentials, PVC for `/var/mail/vhosts`.
- [ ] `terminationGracePeriodSeconds` computed: `sessionGracePeriod + killTimeout + 20`.
- [ ] Service: one ClusterIP/LoadBalancer exposing the public ports (993, 995, 465, 587, 24, 143, 110).
- [ ] `strategy: Recreate` (single PVC, RWO).
- [ ] Resources: requests/limits per `ARCHITECTURE.md` §Sizing.
- [ ] Dockerfile: `alpine:3.23` base, multi-stage, `-ldflags="-s -w" -trimpath`, `linux/amd64` only.
- [ ] `helm lint helm/yarilo-standalone` in CI.

## Phase 7 — Tests

- [ ] Smoke checks in `app/smoketest`: IMAP TLS greeting, POP3 TLS greeting, Submission EHLO+STARTTLS, LMTP LHLO, every `/healthz` and `/readyz`.
- [ ] `smoketest-e2e`: full mail roundtrip — deliver via LMTP → read via IMAP → assert content and headers.
- [ ] e2e: AUTH success/failure paths (passdb hit/miss + cache).
- [ ] e2e: IDLE on one process + APPEND on another (via embedded-locks EVENT) → notification arrives.
- [ ] e2e: rate-limit enforcement (anvil) — connection N+1 is refused.
- [ ] e2e: TLS rotation — reload cert without dropping existing sessions.
- [ ] CI: `go test ./...`, `golangci-lint`, `helm lint`, `hadolint`.

## Phase 8 — Documentation + release

- [ ] `INSTALL.md`: standalone deploy walkthrough (k8s manifest, cert-manager, PVC sizing).
- [ ] Per-binary docs in `docs/` — env vars, CLI flags, config sections.
- [ ] `README.md`: mark standalone-ready components ✅; add a quick-start standalone example.
- [ ] `helm/Chart.yaml`: bump `appVersion` to `v0.1.0` (Phase 1 milestone from `PLAN.md`).
- [ ] CI release pipeline (auto on `appVersion` bump): git-cliff CHANGELOG, GHCR tag, GitHub release.

---

## Cross-cutting checks after every phase

- `golangci-lint run` green.
- `go test ./...` green.
- `helm lint helm/yarilo-standalone` green (from Phase 6).
- Smoke test on staging green (from Phase 7).
- Every new package has `*_test.go` in the same commit.
- No `init()`, no `log.Printf`, no `fork()`, no `syscall.Setuid`.

## Dependency graph

```
Phase 0 (locks foundation)
   ↓
Phase 1 (storage migration to locks)
   ↓
Phase 2 (session processes)  ┐
Phase 4 (shared services)    ┼─→ Phase 3 (login proxies)
                              ↓
                         Phase 5 (lifecycle)
                              ↓
                         Phase 6 (Helm + Docker)
                              ↓
                         Phase 7 (smoke + e2e)
                              ↓
                         Phase 8 (docs + release v0.1.0)
```

Phase 0 + 1 are blockers (locks). Phase 2 and Phase 4 run in parallel (separate binaries, no inter-dependencies). Phase 3 waits on Phase 2 (needs session processes to proxy to). Phases 5–8 are sequential.

## Reality check

Several Phase 2–4 components are already partially implemented — binaries exist in `app/`, recent PRs cover anvil-login-client, LMTP fan-out, and director session routing. Before kicking off each phase, do a quick component audit: gap list (done / stubbed / missing) to avoid redoing what already works.
