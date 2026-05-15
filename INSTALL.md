# Installing yarilo

End-to-end deploy of the yarilo mail server onto a Kubernetes cluster (microk8s, k3s, EKS, GKE — any). This guide targets the sandbox release `mail-sb.seconddns.com`; substitute your own hostname / namespace anywhere they appear.

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
mail-sb.seconddns.com  A  <LB-IP>
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

The SecondDNS workspace already ships a ready-made ClusterIssuer at [k8s_relay/clusterissuer-patch.yaml](../k8s_relay/clusterissuer-patch.yaml). It supports both solvers:

| Solver | When it's used | Use case |
|:---|:---|:---|
| `http01` (nginx ingress) | default | web services that expose port 80 |
| `dns01` (Cloudflare) | when `dnsZones` includes the target | mail (no port 80 needed), wildcards |

For `mail-sb.seconddns.com` the DNS01 solver fires automatically because the zone selector matches `seconddns.com`.

Apply if not yet present:

```sh
kubectl apply -f /path/to/k8s_relay/clusterissuer-patch.yaml

# The Cloudflare API token Secret the issuer references:
kubectl -n cert-manager get secret cloudflare-api-token \
  || kubectl -n cert-manager create secret generic cloudflare-api-token \
       --from-literal=api-token=<CLOUDFLARE-API-TOKEN>

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

The init script splits authentication from mailbox profile (Dovecot-style):

| Table | Role | Columns |
|:---|:---|:---|
| `auth_users` | passdb (authentication) | `email`, `password`, `active`, `created_at`, `updated_at` |
| `mail_users` | userdb (mailbox profile) | `email` (FK), `home`, `mail_loc`, `quota_bytes` |

`mail_users.email` references `auth_users.email` with `ON DELETE CASCADE`, so deleting an auth record automatically drops its mailbox metadata. `values-sandbox.yaml` wires this up via three queries: `passwordQuery` (auth check), `userQuery` (post-auth home / mail lookup), and `iterateQuery` (list active users).

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
dig +short mail-sb.seconddns.com
```

---

## Step 5 — Seed a test user

A test user lives in two tables — credentials in `auth_users`, mailbox profile in `mail_users`. Both rows must exist:

```sh
EMAIL="alice@mail-sb.seconddns.com"
PASS="wonderland"
HASH="$(htpasswd -nbB alice "$PASS" | cut -d: -f2)"

kubectl -n yarilo-sb exec -i yarilo-postgres-0 -- \
  psql -U yarilo -d yarilo <<SQL
INSERT INTO auth_users (email, password, active)
  VALUES ('${EMAIL}', '{BCRYPT}${HASH}', TRUE);

INSERT INTO mail_users (email)
  VALUES ('${EMAIL}');
SQL
```

Verify:

```sh
kubectl -n yarilo-sb exec yarilo-postgres-0 -- \
  psql -U yarilo -d yarilo -c \
  "SELECT a.email, a.active, m.home, m.mail_loc
   FROM auth_users a LEFT JOIN mail_users m USING (email);"
```

Leaving `home` and `mail_loc` empty is intentional — the Maildir backend currently derives the path from the email itself (`<maildirRoot>/<domain>/<local-part>`) regardless of what userdb returns. The `mail_users` row still has to exist so `userQuery` finds it; populate `home`/`mail_loc` later when per-user overrides become useful (e.g. moving heavy users onto a different volume).

---

## Step 6 — Smoke test

The `app/smoketest-e2e` binary drives the full mail flow (Submission AUTH PLAIN + LOGIN, LMTP deliver, IMAPS LOGIN + AUTHENTICATE PLAIN, POP3S USER/PASS + AUTH PLAIN). From the repo:

```sh
go run ./app/smoketest-e2e/ \
  -host mail-sb.seconddns.com \
  -user alice@mail-sb.seconddns.com \
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
