# Installing yarilo

yarilo installs two ways. Pick the one that matches your target:

| Path | Best for | Topology | Passdb |
|:---|:---|:---|:---|
| **[A. Docker Compose](#part-a--docker-compose)** | one host — local dev, evaluation, small self-hosted installs | standalone (no director), all roles as containers on one host | SQLite (self-contained) |
| **[B. Kubernetes](#part-b--kubernetes)** | production, HA, multi-node | director ring + co-located backend, login proxies, shared auth / warden / locks | MySQL (default) or Postgres |

Both run the **same image** (`ghcr.io/yarilomail/yarilo`); a component selects its
role via `YARILO_COMPONENT` (Compose) or the Helm chart (Kubernetes). Clients
always connect to **login proxies** that terminate TLS and forward plaintext to
the session backends.

Examples use `mail.example.com` and namespace `yarilo-sb` — substitute your own
values throughout.

---

# Part A — Docker Compose

A single-host **standalone** deployment. Clients hit login proxies (TLS
terminators) that forward to session backends; the backends share a Maildir
store and coordinate through `yarilo-auth`, `yarilo-warden` and `yarilo-locks`
(Redis-backed). The userdb is SQLite — **no external database required.**

> Single-host, **not** highly available. For HA / multi-node use Part B.
> Everything lives in [`deploy/compose/`](deploy/compose); this section is the
> quickstart — see [docs/DOCKER-COMPOSE.md](docs/DOCKER-COMPOSE.md) for MTA
> integration, operations and the full service reference.

## A.1 Prerequisites

- Docker Engine 24+ with the Compose v2 plugin (`docker compose version`).
- Host ports free: 143/993, 110/995, 587/465, 4190 (and, loopback-only by
  default, 24 / 12325 / 12340). Override in `.env`.
- ~512 MB RAM.

## A.2 Quickstart

```sh
cd deploy/compose
cp .env.example .env                 # adjust image tag / ports if needed
./gen-certs.sh mail.example.com      # self-signed TLS for local use
docker compose up -d
docker compose ps                    # all containers report "healthy"
```

`gen-certs.sh` writes a self-signed `tls/cert.pem` + `tls/key.pem` (the key is
world-readable so the uid-1000 container can load it). For a public deployment,
drop a real cert/key (e.g. Let's Encrypt `fullchain.pem` / `privkey.pem`) into
`deploy/compose/tls/` as `cert.pem` / `key.pem` and restart the login proxies —
`tls/` is git-ignored.

## A.3 Create the first user

The SQLite userdb lives in the `state` volume at `/var/lib/yarilo/users.db`;
`yarilo-auth` creates the `yarilo_users` table on first start. Insert a user with
a one-off sqlite container. `{PLAIN}` is fine for local testing only — use a
hashed scheme (`{BCRYPT}…`) for anything real. The `mail` column is the per-user
mail location (`maildir:` / `sdbox:` / `mdbox:`):

```sh
docker run --rm -v yarilo_state:/data nouchka/sqlite3 /data/users.db \
  "INSERT INTO yarilo_users (username,password,home,mail,enabled) VALUES \
   ('user@mail.example.com','{PLAIN}changeit', \
    '/var/mail/vhosts/mail.example.com/user@mail.example.com', \
    'maildir:/var/mail/vhosts/mail.example.com/user@mail.example.com',1);"
```

## A.4 Send and read a test message

```sh
# Deliver via LMTP (what your MTA does), loopback:
printf 'LHLO t\r\nMAIL FROM:<s@ext.test>\r\nRCPT TO:<user@mail.example.com>\r\nDATA\r\nSubject: hi\r\n\r\nhello\r\n.\r\nQUIT\r\n' \
  | nc 127.0.0.1 24

# Read over IMAPS (self-signed cert → the verify warning is expected):
printf 'a login user@mail.example.com changeit\r\nb select INBOX\r\nc logout\r\n' \
  | openssl s_client -quiet -connect 127.0.0.1:993
```

Or run the bundled smoke test (the entrypoint dispatches on `YARILO_COMPONENT`,
so invoke the binary directly):

```sh
docker compose exec yarilo-imap \
  yarilo-smoketest -host yarilo-imap-login -imap-port 10993 \
  -telemetry http://yarilo-imap-login:8080 -insecure=true
```

## A.5 Ports

| Service | Host port | Notes |
|:---|:---|:---|
| IMAP / IMAPS | 143 / 993 | login proxy, STARTTLS / implicit TLS |
| POP3 / POP3S | 110 / 995 | login proxy |
| Submission | 587 / 465 | login proxy |
| ManageSieve | 4190 | login proxy |
| LMTP | 24 (loopback) | **unauthenticated** — for your MTA only |
| SASL auth | 12325 (loopback) | fronting MTA: `smtpd_sasl_type = dovecot` |
| Quota policy | 12340 (loopback) | fronting MTA: `check_policy_service` |

Wiring a fronting Postfix (LMTP delivery, SASL auth, quota policy) is covered in
[docs/DOCKER-COMPOSE.md §5](docs/DOCKER-COMPOSE.md).

## A.6 Operations

- **Logs**: `docker compose logs -f yarilo-imap` (any service); `LOG_LEVEL=debug`
  in `.env` for verbose output.
- **Upgrade**: bump `YARILO_TAG` in `.env`, then `docker compose pull && docker compose up -d`.
- **Backup**: snapshot the `mail` (messages + indexes) and `state` (userdb) volumes.
- **Uninstall**: `docker compose down`; add `-v` to also delete the `mail`,
  `state` and `redis` volumes (destroys all data).

---

# Part B — Kubernetes

The production topology: a **director** ring (StatefulSet) routes each user to a
single **co-located backend** pod that carries every protocol, fronted by
per-protocol **login proxies**, with shared `yarilo-auth`, `yarilo-warden` and
`yarilo-locks` (Redis-backed) plus `yarilo-quota-status`. See
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) and the diagrams
[yarilo_director.svg](docs/yarilo_director.svg) /
[yarilo_backend.svg](docs/yarilo_backend.svg) for the full rationale.

This section uses the sandbox example (`helm_values/values-sandbox.yaml`, a
single-node microk8s namespace `yarilo-sb`). Production notes ("swap X") are
called out inline.

## What you'll end up with

| Component | Kind | Purpose |
|:---|:---|:---|
| `yarilo-director` | StatefulSet | LMTP proxy + consistent-hash ring, owns backend routing |
| `yarilo-backend` | StatefulSet | co-located pod: imap / pop3 / submission / lmtp / managesieve + fts + reg sidecar |
| `yarilo-*-login` | Deployments | TLS-terminating login proxies (imap / pop3 / submission / lmtp / managesieve / sasl) |
| `yarilo-auth` | Deployment | passdb + userdb against the SQL store |
| `yarilo-warden` | Deployment | connection accounting, auth-penalty, cross-node kick bus |
| `yarilo-locks` | Deployment | cross-process write coordination (mTLS `:9104`, Redis-backed) |
| `yarilo-quota-status` | Deployment | RCPT-time quota policy service for a fronting MTA |
| SQL StatefulSet | StatefulSet | passdb / userdb store (sandbox only — use managed SQL in production) |
| `yarilo-tls` Secret | Secret | TLS cert provisioned by cert-manager |
| LoadBalancer Services | Service | public entry per login proxy (143/993, 110/995, 587/465, 4190, 24) |

## B.1 Prerequisites

### DNS

An A (or AAAA) record for the public hostname pointing at the LoadBalancer IP
your cluster will assign:

```
mail.example.com  A  <LB-IP>
```

For real mail you also need MX, SPF/TXT, DKIM, PTR. For the sandbox an A record
is enough — the smoke test connects by hostname.

### cert-manager

```sh
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.16.x \
  --set installCRDs=true
kubectl -n cert-manager get pods          # all Running
```

### `letsencrypt-prod` ClusterIssuer

The repo ships `deploy/clusterissuer.yaml` with two solvers — `http01` (nginx
ingress) and `dns01` (Cloudflare, used for mail since no port 80 is needed).
Edit it (replace `admin@example.com` / `example.com`), then apply:

```sh
kubectl -n cert-manager create secret generic cloudflare-api-token \
  --from-literal=api-token=<CLOUDFLARE-API-TOKEN>
kubectl apply -f deploy/clusterissuer.yaml
kubectl get clusterissuer letsencrypt-prod     # STATUS: Ready
```

### LoadBalancer support

The chart provisions `Service` objects of type `LoadBalancer`. Ensure a
controller can fulfil them:

- **microk8s** — `microk8s enable metallb` and assign an IP range.
- **k3s** — bundled `klipper-lb` works out of the box.
- **EKS/GKE/AKS** — the cloud LB controller handles it automatically.

## B.2 Passdb / userdb store

yarilo-auth resolves logins from a SQL store. Two sandbox manifests ship —
**MySQL is the default** wired by `values-sandbox.yaml`; **Postgres is an
alternative**. Both put login (passdb) and storage/quota/routing (userdb) in the
same store; the split into `password_query` / `user_query` / `iterate_query`
lives in `values-sandbox.yaml`, so migrating to a production layout only changes
the queries.

> **Production:** swap the sandbox manifest for a managed SQL DSN — replace the
> `dsn` key in the Secret and run the init SQL against the managed instance. The
> Secret reference in `values-sandbox.yaml` stays the same.

### Option 1 — MySQL (default)

`helm_values/mysql-sandbox.yaml` creates namespace `db`, a `mysql` Secret
(DB `yarilo`, user `yarilo` / `sandbox-secret`), an init ConfigMap and a MySQL 8.4
StatefulSet. The schema is the PostfixAdmin layout — `domain`, `alias` and
`mailbox` tables:

```sh
kubectl apply -f helm_values/mysql-sandbox.yaml
kubectl -n db get pods,svc,pvc
```

The `mailbox` table is what `values-sandbox.yaml` queries (columns matched by
name; `password` is the only required one, `active` maps to `enabled`). Its DSN
reaches yarilo-auth through the `YARILO_DB_DSN` env var, injected from the `mysql`
Secret — `skip_schema: true` keeps yarilo-auth from creating its own table.

Seed a user (`domain` + `mailbox` + `alias` rows):

```sh
kubectl -n db exec -i mysql-0 -- mysql -uyarilo -psandbox-secret yarilo <<'SQL'
INSERT IGNORE INTO domain (domain, transport, customer_id, active)
VALUES ('mail.example.com', 'virtual', 'cust-0001', 1);

INSERT IGNORE INTO mailbox
  (username, password, mbtype, home, mpath, maildir, local_part, domain, active)
VALUES
  ('alice@mail.example.com', '{BCRYPT}REPLACE_WITH_HASH', 'mdbox',
   '/var/mail/vhosts/', 'mail.example.com/alice@mail.example.com', 'mdbox',
   'alice', 'mail.example.com', 1);

INSERT IGNORE INTO alias (address, goto, domain, active)
VALUES ('alice@mail.example.com', 'alice@mail.example.com', 'mail.example.com', 1);
SQL
```

Generate the hash with `htpasswd -nbB alice '<password>' | cut -d: -f2` and
prefix it with `{BCRYPT}`.

### Option 2 — Postgres (alternative)

`helm_values/postgres-sandbox.yaml` creates a `yarilo-postgres` StatefulSet
(PG 16, user `yarilo` / `sandbox-secret`) with a **single `users` table**
covering passdb (`username` / `password` / `active`) and userdb (`home` /
`mail_path` / quota / `allow_nets` / `director_tag`):

```sh
kubectl apply -f helm_values/postgres-sandbox.yaml
kubectl -n yarilo-sb get pods,svc,pvc
```

To use Postgres instead of MySQL, point yarilo-auth at this store **and switch
the passdb block** in `values-sandbox.yaml` to `driver: postgres` with the
`users`-table queries (the `postgres-sandbox.yaml` header documents the columns
and the expected `password_query` / `user_query` / `iterate_query`). Seed a user:

```sh
kubectl -n yarilo-sb exec -i yarilo-postgres-0 -- \
  psql -U yarilo -d yarilo -c \
  "INSERT INTO users (username, password, active, display_name)
   VALUES (LOWER('alice@mail.example.com'), '{BCRYPT}REPLACE_WITH_HASH', TRUE, 'Alice');"
```

`home` / `mail_path` left blank → yarilo derives the path from the
`storage.mailHomeTemplate` (`%d/%n` → `<maildirRoot>/<domain>/<local-part>`). Set
`home` to an absolute path to override per user.

## B.3 Deploy yarilo

```sh
helm upgrade --install yarilo ./helm \
  -f helm_values/values-sandbox.yaml \
  -n yarilo-sb
```

Listeners brought up (via the login-proxy LoadBalancer Services):

| Listener | Port | TLS mode |
|:---|:---|:---|
| IMAPS / IMAP | 993 / 143 | implicit TLS / STARTTLS |
| Submissions / Submission | 465 / 587 | implicit TLS / STARTTLS |
| POP3S / POP3 | 995 / 110 | implicit TLS / STARTTLS |
| LMTP | 24 | plain (internal — front it with an MTA) |
| Telemetry | 8080 | ClusterIP only — `/healthz`, `/readyz`, `/metrics` |

## B.4 Wait for the certificate

cert-manager creates a `Certificate`, runs the DNS01 challenge, and writes the
cert into the `yarilo-tls` Secret mounted by the login proxies (no restart
needed once populated):

```sh
kubectl -n yarilo-sb get certificate yarilo-tls -w      # wait for READY=True
kubectl -n yarilo-sb describe certificate yarilo-tls    # if it hangs
```

First issuance typically takes 1–3 minutes.

## B.5 Get the LoadBalancer IP

Each login proxy has its own LoadBalancer Service; the IMAP one is the usual
public entry:

```sh
kubectl -n yarilo-sb get svc yarilo-imap-login -w       # wait for EXTERNAL-IP
dig +short mail.example.com                              # confirm DNS resolves to it
```

## B.6 Smoke test

`app/smoketest-e2e` drives the full flow (Submission AUTH, LMTP deliver, IMAPS
LOGIN + SASL, POP3S). From the repo against the public host:

```sh
go run ./app/smoketest-e2e/ \
  -host mail.example.com \
  -user alice@mail.example.com \
  -pass '<password>' \
  -submission-port 587 -lmtp-port 24 -imaps-port 993 -pop3s-port 995
```

For an in-cluster protocol/sieve check, the `yarilo-smoketest` binary runs as a
Job (see `hack/smoketest/job.yaml`) against the login-proxy Services.

---

## Storage architecture and phase roadmap

yarilo storage follows the reference `mail_storage` pattern: `MailboxBackend` and
`IndexBackend` are per-process factories; `UserMailbox` / `UserIndex` are
per-session handles created once after authentication and closed when the session
ends. All per-user state (filesystem root, quota, uid/gid) is captured in
`mailbox.UserInfo` at login time.

### Currently implemented

| Mechanism | Status |
|:---|:---|
| Per-user handle (`MailboxBackend.OpenUser`) | ✅ |
| `Resolver` — `%d/%n` template → absolute home | ✅ |
| Backends: maildir, dbox, mdbox | ✅ |
| Index: fileindex (yarilo-uidlist v3) | ✅ |
| userdb-driven `home` override | ✅ |

The global home layout is controlled by two config keys:

```yaml
storage:
  maildirRoot: /var/mail/vhosts      # prepended to template-derived paths
  mailHomeTemplate: "%d/%n"          # %d=domain, %n=local-part, %u=full email
```

Default (`%d/%n`) gives `/var/mail/vhosts/example.com/alice`. Use `%u` for a flat
layout, or `%n` for single-domain setups. Per-user relocation is a DB-only change
(populate `home`), no code or config change needed.

### On the roadmap

- **Per-user mailbox format and namespaces** — `mbtype` (MySQL) / `mailbox_format`
  (Postgres) selects a per-user backend; `MailboxBackend.OpenNamespace` will root
  shared / public namespaces from a future `config.Namespaces` block.
- **Quota enforcement** — `quota_bytes` populates `UserInfo`, enforced at
  `UserMailbox.Save`; IMAP and LMTP translate the typed error to `OVERQUOTA` /
  `452`.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|:---|:---|:---|
| `Certificate` stuck `Issuing` | Cloudflare API token missing/expired | `kubectl -n cert-manager describe order …`; recreate `cloudflare-api-token` |
| login pod CrashLoopBackOff at startup | `YARILO_DB_DSN` resolves empty | check the SQL Secret's `dsn` key is populated and base64-encoded |
| LoadBalancer stuck `<pending>` | no LB controller / metallb not enabled | enable metallb or set `service.type=NodePort` for the sandbox |
| Smoke test `tls: handshake failure` | DNS not yet at the LB, or cert still issuing | `dig` the host, `kubectl get certificate`, wait |
| AUTH fails for the seeded user | wrong table/column or missing `{SCHEME}` prefix | MySQL: check `mailbox` row `active=1`; Postgres: check `users` row `active=TRUE`; verify the `{BCRYPT}` prefix |

Logs (per component — there is no single `yarilo` Deployment):

```sh
kubectl -n yarilo-sb logs deploy/yarilo-imap-login --tail=200 -f
kubectl -n yarilo-sb logs deploy/yarilo-auth --tail=200 -f
```

`LOG_LEVEL=debug` is on by default in `values-sandbox.yaml`.

## Uninstall

```sh
helm uninstall yarilo -n yarilo-sb
kubectl delete -f helm_values/mysql-sandbox.yaml       # or postgres-sandbox.yaml
```

PVCs (backend maildir, SQL data) are deleted with their namespaces. To keep mail
data, take a `kubectl cp` snapshot first.
