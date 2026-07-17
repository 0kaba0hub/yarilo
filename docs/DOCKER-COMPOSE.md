# Running yarilo with Docker Compose

A single-host deployment for local development, evaluation and small self-hosted
installs. It runs the whole mail server as **one `yarilo` process** in standalone
mode (`mode: single`): every protocol (IMAP, POP3, LMTP, Submission,
ManageSieve) plus embedded auth, anvil and locks, in-process.

The **minimal** stack has no external dependencies — a SQLite userdb and local
storage volumes. The **full** profile adds MariaDB (SQL userdb) and Redis (dict /
quota-clone state).

> This is a single-host target and is **not** highly available. For HA /
> multi-node use the Helm chart (see [DEPLOYMENT.md](DEPLOYMENT.md)).

All files live in [`deploy/compose/`](../deploy/compose).

---

## 1. Prerequisites

- Docker Engine 24+ with the Compose v2 plugin (`docker compose version`).
- BuildKit is not required (the published image is pulled, not built).
- Host ports 143/993/110/995/587/465/24/4190/8080 free (override in `.env`).
- ~256 MB RAM for the minimal stack.

## 2. Quickstart (minimal)

```sh
cd deploy/compose
cp .env.example .env                 # adjust the image tag / ports if needed
./gen-certs.sh mail.example.test     # self-signed TLS for local use
docker compose up -d
docker compose logs -f yarilo        # watch it come up
curl -fsS http://127.0.0.1:8080/healthz && echo OK
```

### Create the first user (SQLite)

The SQLite userdb lives in the `state` volume at `/var/lib/yarilo/users.db`;
yarilo creates the `yarilo_users` table on first start. Insert a user with a
one-off sqlite container (password `{PLAIN}...` is fine for local testing only):

```sh
docker run --rm -v yarilo_state:/data nouchka/sqlite3 /data/users.db \
  "INSERT INTO yarilo_users (username,password,home,mail,enabled) VALUES \
   ('user@example.test','{PLAIN}changeit', \
    '/var/mail/vhosts/example.test/user@example.test', \
    'maildir:/var/mail/vhosts/example.test/user@example.test',1);"
```

### Send and read a test message

```sh
# Deliver via LMTP (what your MTA would do)
printf 'LHLO t\r\nMAIL FROM:<s@ext.test>\r\nRCPT TO:<user@example.test>\r\nDATA\r\nSubject: hi\r\n\r\nhello\r\n.\r\nQUIT\r\n' \
  | nc 127.0.0.1 24

# Read over IMAPS (self-signed → -verify_return_error off)
printf 'a login user@example.test changeit\r\nb select INBOX\r\nc logout\r\n' \
  | openssl s_client -quiet -connect 127.0.0.1:993
```

## 3. Full profile (MariaDB + Redis)

```sh
docker compose --profile full up -d
```

`initdb/01-seed.sql` creates the schema and a `user@example.test` /
`{PLAIN}changeit` row in MariaDB on first init. Then switch the auth backend in
`config/yarilo.yaml` from the SQLite block to the MySQL block (commented in the
file) and restart `yarilo`:

```yaml
auth:
  passdb:
    - driver: mysql
      dsn: "yarilo:yarilo-secret@tcp(mariadb:3306)/yarilo"
      password_query: "SELECT password FROM yarilo_users WHERE username = %u AND enabled = 1"
      user_query:     "SELECT home, mail FROM yarilo_users WHERE username = %u"
```

Point dict / quota-clone at the `redis` service (`addr: "redis:6379"`) per
[DICT.md](DICT.md) / [QUOTA.md](QUOTA.md).

## 4. Configuration

| Where | What |
|:---|:---|
| `.env` | image tag, log level, host port mapping, DB credentials |
| `config/yarilo.yaml` | the full server config (mounted read-only) — domain/hostname, storage format, auth backend, enabled services |

- **Mailbox format**: `storage.mailbox` = `maildir` (default), `sdbox` or `mdbox`.
- **Hostname**: set `protocol.submission.hostname` to your real MX name.
- Config keys map 1:1 to the Helm `values.yaml` keys, so migrating between
  Compose and Kubernetes is mechanical.

## 5. TLS

- Local: `./gen-certs.sh <hostname>` writes a self-signed `tls/cert.pem` +
  `tls/key.pem`.
- Production: drop a real certificate/key (e.g. Let's Encrypt `fullchain.pem` /
  `privkey.pem`) into `deploy/compose/tls/` as `cert.pem` / `key.pem` and
  restart. `tls/` is git-ignored.

## 6. DNS / MX for a real deployment

Inbound mail reaches yarilo's LMTP (port 24) from your MTA (Postfix/others), not
directly from the internet. For a public deployment you still need, on the host
that fronts it:

- an **MX** record for the domain pointing at your MTA;
- **SPF** (`v=spf1 ...`), **DKIM** (signing at the MTA) and a **PTR** record for
  the sending IP;
- your MTA configured to LMTP-deliver local recipients to `yarilo:24`.

## 7. Verifying

- `curl http://127.0.0.1:8080/healthz` and `/readyz`.
- IMAP/POP3 greeting via `openssl s_client` (above).
- The smoke test, run as a one-off against the stack:
  ```sh
  docker compose run --rm --no-deps yarilo \
    /usr/local/bin/yarilo-smoketest -host 127.0.0.1 -imap-port 993 \
    -telemetry http://127.0.0.1:8080 -insecure=true
  ```

## 8. Operations

- **Logs**: `docker compose logs -f yarilo`; set `LOG_LEVEL=debug` in `.env` and
  `docker compose up -d` for verbose logging.
- **Upgrade**: bump `YARILO_TAG` in `.env`, then `docker compose pull && docker
  compose up -d`.
- **Backup**: the `mail` (messages + indexes) and `state` (userdb, sieve)
  volumes hold all persistent data — snapshot them with
  `docker run --rm -v yarilo_mail:/d -v "$PWD":/b busybox tar czf /b/mail.tgz -C /d .`.
- **Admin CLI**: `docker compose exec yarilo yarilo-admin ...` (quota, index,
  folder, who). **Migrate** formats with `yarilo-migrate`.

## 9. Troubleshooting

| Symptom | Check |
|:---|:---|
| container unhealthy | `docker compose logs yarilo`; is `tls/cert.pem` present? |
| auth always fails | user row exists and `enabled=1`; password has a `{SCHEME}` prefix; correct passdb (SQLite vs MySQL) active in config |
| TLS handshake errors | cert CN/SAN matches the hostname you connect to; regenerate with `gen-certs.sh` |
| 0 messages for a user | mailbox format in `config/yarilo.yaml` matches how mail was delivered (maildir vs dbox) |
| LMTP refused | port 24 published and your MTA points at `yarilo:24` |
