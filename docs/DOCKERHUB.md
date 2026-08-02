<!--
Docker Hub Repository Overview for docker.io/0kaba0/yarilo.
Auto-synced on push to main by .github/workflows/dockerhub-description.yaml
(peter-evans/dockerhub-description). The Short Description is set there via
short-description; the repo avatar is uploaded once in the Docker Hub UI from
docs/icon.png (SVG is not accepted). Category: Networking (+ Security).
-->

<table><tr>
<td><img src="https://raw.githubusercontent.com/yarilomail/yarilo/main/docs/icon.png" width="180" alt="yarilo logo"/></td>
<td>

# yarilo

**Production-grade IMAP / POP3 / LMTP / ManageSieve / Submission mail server written in Go, with built-in full-text search.** Multi-binary architecture — each protocol component is a separate process — Kubernetes-native via a Helm chart.

**Source & docs:** https://github.com/yarilomail/yarilo
**License:** AGPL-3.0 · **Platform:** `linux/amd64`

</td>
</tr></table>

## Architecture

Each protocol and infrastructure role is a **separate compiled binary** — no monolith, no mode flags. Login proxies terminate TLS and pass the authenticated connection to session backends; a director ring provides consistent-hash routing with sticky per-user sessions; shared `yarilo-auth` and `yarilo-locks` services back authentication and cross-process write coordination. The same binaries serve a single-node standalone install and a sharded clustered deployment — topology is configuration, not code. See [ARCHITECTURE.md](https://github.com/yarilomail/yarilo/blob/main/docs/ARCHITECTURE.md) and [DEPLOYMENT.md](https://github.com/yarilomail/yarilo/blob/main/docs/DEPLOYMENT.md).

## One image, many components

This is a **single image** containing every yarilo binary. The component to run is selected with the **`YARILO_COMPONENT`** environment variable, so the same image serves the all-in-one process and every clustered role:

| `YARILO_COMPONENT` | Role |
|:---|:---|
| `yarilo` | Standalone all-in-one server |
| `yarilo-imap` / `yarilo-imap-login` | IMAP backend / login proxy |
| `yarilo-pop3` / `yarilo-pop3-login` | POP3 backend / login proxy |
| `yarilo-lmtp` / `yarilo-lmtp-login` | LMTP delivery / login proxy |
| `yarilo-submission` / `yarilo-submission-login` | Submission relay / login proxy |
| `yarilo-managesieve` / `yarilo-managesieve-login` | ManageSieve backend / login proxy |
| `yarilo-sasl-login` | SASL auth socket (Postfix / Exim relay) |
| `yarilo-auth` · `yarilo-warden` · `yarilo-locks` | Shared services (passdb, rate-limit, write locks) |
| `yarilo-director` · `yarilo-backend-api` · `yarilo-backend-reg` | Director ring, admin API, backend registration sidecar |
| `yarilo-fts` · `yarilo-quota-status` · `yarilo-migrate` | Full-text search, quota policy, offline format migration |

## Supported tags

| Tag | Meaning |
|:---|:---|
| `latest` | Latest build from `main`. |
| `X.Y.Z` | A released version (matches the Helm chart `appVersion`). |
| `<short-sha>` | Exact commit build, for pinning. |

## Observability & ports

HTTP on **`8080`** exposes health (`/healthz`, `/readyz`) and Prometheus **`/metrics`**; a `ServiceMonitor` is shipped with the Helm chart. Mail ports (config-driven; defaults):

| Port | Service |
|:---|:---|
| `143` / `993` | IMAP (STARTTLS) / IMAPS |
| `110` / `995` | POP3 / POP3S |
| `587` / `465` | Submission (STARTTLS) / implicit TLS |
| `24` | LMTP |
| `4190` | ManageSieve |

## Environment variables

| Variable | Default | Description |
|:---|:---|:---|
| `YARILO_COMPONENT` | — | **Required.** Selects which binary runs (see table above). |
| `CONFIG` | `/etc/yarilo/yarilo.yaml` | Path to the YAML config, for components that read one. |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` (JSON `slog` output). |
| `POD_IP` | — | The pod's own IP; used by login proxies for session bookkeeping. |
| `TELEMETRY_LISTEN` | `:8080` | Address for the health/metrics HTTP server. |

Admin/API tokens (`YARILO_ADMIN_TOKEN`, `DIRECTOR_API_TOKEN`, …) gate the control-plane endpoints in clustered mode; the Helm chart wires these automatically.

## Deploy

### Kubernetes (recommended)

yarilo is designed for Kubernetes via its Helm chart — login proxies, an optional director ring, co-located backend StatefulSets per storage tag, and shared services. See [DEPLOYMENT.md](https://github.com/yarilomail/yarilo/blob/main/docs/DEPLOYMENT.md).

### Local (standalone)

```sh
docker run --rm \
  -e YARILO_COMPONENT=yarilo \
  -v "$PWD/yarilo.yaml:/etc/yarilo/yarilo.yaml:ro" \
  -p 143:143 -p 993:993 -p 587:587 -p 465:465 \
  0kaba0/yarilo:latest
```

## Stack

- **Go** (`linux/amd64`), multi-stage build → slim runtime
- **Storage:** Maildir / sdbox / mdbox + FileIndex (mail-index v7.3 wire format)
- **Full-text search:** flatcurve (Xapian) via `yarilo-fts`
- **TLS 1.3** throughout; SASL PLAIN / LOGIN / SCRAM-SHA-256 / XOAUTH2; mTLS between components
- **Redis** for the dict / locks state (clustered mode)

## CI

Built and published on every push to `main`; a tagged release is cut automatically when the Helm chart `appVersion` advances. Images are `linux/amd64`, built from [`docker/Dockerfile`](https://github.com/yarilomail/yarilo/blob/main/docker/Dockerfile).
