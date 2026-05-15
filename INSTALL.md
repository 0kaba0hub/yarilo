# Installing yarilo

End-to-end deploy of the yarilo mail server onto a Kubernetes cluster (microk8s, k3s, EKS, GKE — any). Examples use `mail.example.com` and namespace `yarilo-sb` — substitute your own values throughout.

---

## What you'll end up with

| Component | Where | Purpose |
|:---|:---|:---|
| `yarilo` Deployment | `yarilo-sb` namespace | IMAP / Submission / POP3 / LMTP server |
| `yarilo-postgres` StatefulSet | same namespace | Passdb store (sandbox only — production should use managed Postgres) |
| `yarilo-tls` Secret | same namespace | TLS cert provisioned by cert-manager from Let's Encrypt |
| LoadBalancer Service | same namespace | Public entry for 143 / 465 / 587 / 993 / 995 / 110 / 24 |

---

## Prerequisites

### 1. DNS

A record (or AAAA) for the public hostname pointing at the LoadBalancer IP that your cluster will assign:

```
mail.example.com  A  <LB-IP>
```

For real mail you would also need MX, SPF/TXT, DKIM, PTR. For sandbox just the A record is enough — the smoke test connects by hostname.

### 2. cert-manager

cert-manager is the official Jetstack chart:

```sh
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.16.x \
  --set installCRDs=true
```

Verify pods are Running:

```sh
kubectl -n cert-manager get pods
```

### 3. `letsencrypt-prod` ClusterIssuer

The repo ships `deploy/clusterissuer.yaml` — a ready-to-apply ClusterIssuer with two solvers:

| Solver | When it's used | Use case |
|:---|:---|:---|
| `http01` (nginx ingress) | default | web services that expose port 80 |
| `dns01` (Cloudflare) | when `dnsZones` includes the target | mail (no port 80 needed), wildcards |

Edit `deploy/clusterissuer.yaml` — replace `admin@example.com` and `example.com` with your values — then apply:

```sh
# Create the Cloudflare API token Secret first:
kubectl -n cert-manager create secret generic cloudflare-api-token \
  --from-literal=api-token=<CLOUDFLARE-API-TOKEN>

# Apply the ClusterIssuer:
kubectl apply -f deploy/clusterissuer.yaml

kubectl get clusterissuer letsencrypt-prod
```

`STATUS: Ready` means the issuer is healthy.

### 4. LoadBalancer support

The chart provisions a `Service` of type `LoadBalancer`. Make sure your cluster has a controller that can fulfil it:

- **microk8s** — `microk8s enable metallb` and assign an IP range.
- **k3s** — bundled `klipper-lb` works out of the box.
- **EKS/GKE/AKS** — uses the cloud LB controller automatically.

---

## Step 1 — Postgres for passdb and userdb

Apply the sandbox Postgres manifest. It creates:

- the `yarilo-sb` Namespace
- a `yarilo-postgres` Secret (DSN + DB credentials, referenced by both Postgres and yarilo)
- a `yarilo-postgres-init` ConfigMap mounted into `/docker-entrypoint-initdb.d/` — Postgres runs it on first startup to create the schema
- a headless `yarilo-postgres` Service
- a 2 Gi PG 16 `StatefulSet`

```sh
kubectl apply -f helm_values/postgres-sandbox.yaml
kubectl -n yarilo-sb get pods,svc,pvc
kubectl -n yarilo-sb logs yarilo-postgres-0 --tail=30
```

Wait until `yarilo-postgres-0` is `Ready 1/1`. The DSN baked into the Secret is `postgres://yarilo:sandbox-secret@yarilo-postgres:5432/yarilo?sslmode=disable` — yarilo reads it via the `DB_DSN` env var.

### Schema

A single `users` table covers passdb (login + active flag) and userdb (storage / quota / routing):

| Column | Role | Read by yarilo today? |
|:---|:---|:---|
| `username` (PK, lowercase enforced) | login — typically the full email | ✅ |
| `password` | `{SCHEME}hash` — BCRYPT / SHA512-CRYPT / PLAIN | ✅ |
| `active` | gates login | ✅ |
| `home`, `mail_path` | per-user Maildir overrides | ✅ `home` passed to `Resolver.UserInfo` — overrides the global template |
| `mailbox_format` | `''` / `maildir` / `dbox` / `mdbox` — override global default | ❌ forward-compat |
| `quota_bytes` | `0` = unlimited | ❌ forward-compat (quota engine, Phase 10) |
| `allow_nets` | comma-separated CIDRs; `''` = no restriction | ❌ forward-compat (login ACL) |
| `director_tag` | sticky-routing tag for `yarilo-director` | ❌ forward-compat (Phase 5) |
| `display_name`, `last_login_at` | admin / UI | ❌ forward-compat |
| `created_at`, `updated_at` | audit (`updated_at` maintained by trigger) | ❌ admin-only |

`values-sandbox.yaml` wires this up via three queries hitting the same table — `passwordQuery` (auth check), `userQuery` (post-auth home / mail lookup), `iterateQuery` (list active users). The split is intentional: when the time comes to migrate to a multi-table production layout (e.g., auth in an SSO store, profile in SQL), only the queries change. All `%u` placeholders are wrapped in `LOWER()` so callers may use mixed-case emails.

> **For production** swap `helm_values/postgres-sandbox.yaml` for a managed Postgres DSN — replace the `dsn` field in the `yarilo-postgres` Secret and run the init SQL manually against the managed instance. The Secret reference in `values-sandbox.yaml` stays the same.

---

## Step 2 — Deploy yarilo

```sh
helm upgrade --install yarilo ./helm \
  -f helm_values/values-sandbox.yaml \
  -n yarilo-sb
```

What this brings up:

| Listener | Port | TLS mode |
|:---|:---|:---|
| IMAPS | 993 | implicit TLS |
| IMAP | 143 | STARTTLS |
| Submissions | 465 | implicit TLS |
| Submission | 587 | STARTTLS |
| POP3S | 995 | implicit TLS |
| POP3 | 110 | STARTTLS |
| LMTP | 24 | plain (internal — front it with an MTA) |
| Telemetry | 8080 | ClusterIP only — `/healthz`, `/readyz`, `/metrics` |

---

## Step 3 — Wait for the certificate

cert-manager creates a `Certificate` resource based on the values, runs the DNS01 challenge against Cloudflare, then writes the cert into the `yarilo-tls` Secret. Yarilo's Deployment is rolled by Helm's `configChecksum` annotation, but the cert lands in the running pod via the mounted Secret — no restart needed once cert-manager populates it.

```sh
kubectl -n yarilo-sb get certificate yarilo-tls -w
# Wait for READY=True

kubectl -n yarilo-sb describe certificate yarilo-tls  # if it hangs
kubectl -n yarilo-sb get secret yarilo-tls            # populated when cert is ready
```

First issuance typically takes 1–3 minutes (DNS propagation + Let's Encrypt validation).

---

## Step 4 — Get the LoadBalancer IP

```sh
kubectl -n yarilo-sb get svc yarilo -w
```

Once an `EXTERNAL-IP` appears, update DNS if you haven't already and wait for the A record to resolve:

```sh
dig +short mail.example.com
```

---

## Step 5 — Seed a test user

One INSERT, one row:

```sh
USERNAME="alice@mail.example.com"
PASS="wonderland"
HASH="$(htpasswd -nbB alice "$PASS" | cut -d: -f2)"

kubectl -n yarilo-sb exec -i yarilo-postgres-0 -- \
  psql -U yarilo -d yarilo -c \
  "INSERT INTO users (username, password, active, display_name)
   VALUES (LOWER('${USERNAME}'), '{BCRYPT}${HASH}', TRUE, 'Alice');"
```

Verify:

```sh
kubectl -n yarilo-sb exec yarilo-postgres-0 -- \
  psql -U yarilo -d yarilo -c \
  "SELECT username, active, mailbox_format, quota_bytes, display_name FROM users;"
```

`home` / `mail_path` left blank — yarilo uses the global `storage.mailHomeTemplate` (`%d/%n` by default) to derive the path: `<maildirRoot>/<domain>/<local-part>`. Set `home` to an absolute path in the `users` table to override the template for a specific user.

`mailbox_format`, `allow_nets`, `director_tag`, `quota_bytes` are also intentionally default — yarilo doesn't read them yet (see the schema table above). They're in the table so future yarilo releases that wire those features won't require a schema migration.

---

## Step 6 — Smoke test

The `app/smoketest-e2e` binary drives the full mail flow (Submission AUTH PLAIN + LOGIN, LMTP deliver, IMAPS LOGIN + AUTHENTICATE PLAIN, POP3S USER/PASS + AUTH PLAIN). From the repo:

```sh
go run ./app/smoketest-e2e/ \
  -host mail.example.com \
  -user alice@mail.example.com \
  -pass wonderland \
  -submission-port 587 \
  -lmtp-port 24 \
  -imaps-port 993 \
  -pop3s-port 995
```

Expected output:

```
[PASS] submission AUTH PLAIN over STARTTLS
[PASS] submission AUTH LOGIN over STARTTLS
[PASS] LMTP deliver to mailbox
[PASS] IMAPS LOGIN command
[PASS] IMAPS AUTHENTICATE PLAIN (SASL)
[PASS] POP3S USER/PASS
[PASS] POP3S AUTH PLAIN (SASL)
```

LMTP on port 24 is normally not exposed publicly — for the sandbox the LoadBalancer Service publishes it so you can test end-to-end. For production, restrict it via `Service.spec.loadBalancerSourceRanges` or move LMTP behind an MTA.

---

## Storage architecture and phase roadmap

yarilo storage follows the Dovecot `mail_storage` pattern: `MailboxBackend` and
`IndexBackend` are per-process factories; `UserMailbox` / `UserIndex` are
per-session handles created once after authentication and closed when the
session ends. All per-user state (filesystem root, future quota, uid/gid) is
captured in `mailbox.UserInfo` at login time.

### Currently implemented

| Mechanism | Status |
|:---|:---|
| Per-user handle (`MailboxBackend.OpenUser`) | ✅ Phase 2 |
| `Resolver` — `%d/%n` template → absolute home | ✅ Phase 2 |
| Backends: maildir, dbox, mdbox | ✅ Phase 2 |
| Index: fileindex (dovecot-uidlist v3) | ✅ Phase 2 |
| `UserInfo.Home` used by storage, not derived in backend | ✅ Phase 2 |

### Phase 3 — userdb-driven home override ✅

Already implemented. The auth layer (`internal/auth/sql`) returns `home` from
the passdb/userdb query result; each protocol server passes it to
`Resolver.UserInfo(username, res.Home)`. When `home` is non-empty yarilo uses
it verbatim (absolute path) or joins it with `Root` (relative path), skipping
the template entirely. When it is empty the `%d/%n` template fires as usual.

The **global** home layout is controlled by two config keys:

```yaml
storage:
  maildirRoot: /var/mail/vhosts        # Root prepended to template-derived paths
  mailHomeTemplate: "%d/%n"            # Dovecot %-vars: %d=domain, %n=local, %u=full email
```

Default (`%d/%n`) gives `/var/mail/vhosts/example.com/alice`. Switch to `%u`
for a flat layout (`/var/mail/vhosts/alice@example.com`), or `%n` for
single-domain setups.

Per-user relocation is a DB-only change — populate the `home` column in the
`users` table, no code or config change needed.

### Phase 5 — per-user mailbox format and namespaces

The `mailbox_format` column in the `users` table is reserved for selecting a
per-user backend (`maildir` / `dbox` / `mdbox`) instead of the global
`storage.mailbox` config value. The `OpenNamespace` stub in the
`MailboxBackend` / `IndexBackend` interfaces is placeholder for shared / public
namespace support.

**Pending:** `MailboxBackend.OpenNamespace(user *UserInfo, ns string)` — returns
a handle for a non-private namespace rooted at the namespace storage dir.
Namespace list (private / shared / public, each with its own location template)
comes from a future `config.Namespaces` block. No schema migration needed.

### Phase 10 — quota enforcement

`UserInfo.QuotaBytes` field (stub, commented out) will be populated from the
`quota_bytes` column and enforced at `UserMailbox.Save` time. The storage layer
returns a typed error; IMAP and LMTP translate it to `OVERQUOTA` / `452`.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|:---|:---|:---|
| `Certificate` stuck `Issuing` | Cloudflare API token missing/expired | `kubectl -n cert-manager describe order …` and recreate `cloudflare-api-token` Secret |
| Pod CrashLoopBackOff at startup | `DB_DSN` env var resolves to empty | `kubectl -n yarilo-sb get secret yarilo-postgres -o yaml` — the `dsn` key must be base64-encoded |
| `helm install` complains about `protocol.smtp` | Older chart cached locally | `helm repo update` / clear `~/.cache/helm` |
| LoadBalancer stuck `<pending>` | No LB controller / metallb not enabled | Enable metallb or set `service.type=NodePort` for sandbox |
| Smoke test `tls: handshake failure` | DNS not yet pointing at LB, or cert still issuing | `dig` the host, `kubectl get certificate`, wait |
| AUTH fails with the seeded user | Wrong column / encoding in DB | `psql … "SELECT username, password FROM yarilo_users;"` and verify the `{BCRYPT}` prefix |

Logs:

```sh
kubectl -n yarilo-sb logs deploy/yarilo --tail=200 -f
```

`LOG_LEVEL=debug` is on by default in `values-sandbox.yaml` — protocol-level traces show in stdout.

---

## Uninstall

```sh
helm uninstall yarilo -n yarilo-sb
kubectl delete -f helm_values/postgres-sandbox.yaml
```

The `yarilo` PVC (10 Gi maildir) and the Postgres PVC (2 Gi) are deleted with the namespace. If you want to keep the mail data, take a `kubectl cp` snapshot first.
