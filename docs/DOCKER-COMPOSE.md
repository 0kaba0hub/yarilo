# Running yarilo with Docker Compose

A single-host **standalone** deployment for local development, evaluation and
small self-hosted installs. It mirrors the standalone topology from the Helm
chart, on one host, with Docker Compose.

Clients connect to **login proxies** (which terminate TLS) that forward to the
**session backends**; the backends share a Maildir store and coordinate through
`yarilo-auth`, `yarilo-warden` and `yarilo-locks` (Redis-backed). One image serves
every role — each container picks its component via `YARILO_COMPONENT`.

> Single-host, **not** highly available. For HA / multi-node use the Helm chart
> ([DEPLOYMENT.md](DEPLOYMENT.md)).

Everything lives in [`deploy/compose/`](../deploy/compose).

## Services

| Group | Containers |
|:---|:---|
| Infra | `redis`, `yarilo-auth` (userdb), `yarilo-warden`, `yarilo-locks` |
| Session backends | `yarilo-imap`, `yarilo-pop3`, `yarilo-lmtp`, `yarilo-submission`, `yarilo-managesieve`, `yarilo-jmap` |
| Login proxies (TLS) | `yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-login`, `yarilo-managesieve-login`, `yarilo-jmap-login` |
| MTA integration | `yarilo-sasl-login` (SASL auth for Postfix), `yarilo-quota-status` (quota policy) |

The userdb is SQLite (`yarilo-auth` owns it in the `state` volume); mail lives in
the shared `mail` volume. No external database is required.

## 1. Prerequisites

- Docker Engine 24+ with the Compose v2 plugin (`docker compose version`).
- Host ports free: 143/993, 110/995, 587/465, 4190, 8443 (and, loopback-only by
  default, 24 / 12325 / 12340). Override in `.env`.
- ~512 MB RAM.

## 2. Quickstart

```sh
cd deploy/compose
cp .env.example .env                 # adjust image tag / ports if needed
./gen-certs.sh mail.example.test     # self-signed TLS for local use
docker compose up -d
docker compose ps                    # all containers healthy
```

### Create the first user

The SQLite userdb lives in the `state` volume at `/var/lib/yarilo/users.db`;
`yarilo-auth` creates the `yarilo_users` table on first start. Insert a user with
a one-off sqlite container (`{PLAIN}` is fine for local testing only — use a
hashed scheme for real use). The `mail` column is the per-user `mail_location`
(`maildir:` / `sdbox:` / `mdbox:`):

```sh
docker run --rm -v yarilo_state:/data nouchka/sqlite3 /data/users.db \
  "INSERT INTO yarilo_users (username,password,home,mail,enabled) VALUES \
   ('user@example.test','{PLAIN}changeit', \
    '/var/mail/vhosts/example.test/user@example.test', \
    'maildir:/var/mail/vhosts/example.test/user@example.test',1);"
```

### Send and read a test message

```sh
# Deliver via LMTP (what your MTA does), loopback:
printf 'LHLO t\r\nMAIL FROM:<s@ext.test>\r\nRCPT TO:<user@example.test>\r\nDATA\r\nSubject: hi\r\n\r\nhello\r\n.\r\nQUIT\r\n' \
  | nc 127.0.0.1 24

# Read over IMAPS (self-signed cert → the verify warning is expected):
printf 'a login user@example.test changeit\r\nb select INBOX\r\nc logout\r\n' \
  | openssl s_client -quiet -connect 127.0.0.1:993
```

## 3. Ports

| Service | Host port | Notes |
|:---|:---|:---|
| IMAP / IMAPS | 143 / 993 | login proxy, STARTTLS / implicit TLS |
| POP3 / POP3S | 110 / 995 | login proxy |
| Submission | 587 / 465 | login proxy |
| ManageSieve | 4190 | login proxy |
| JMAP | 8443 | login proxy, HTTPS |
| LMTP | 24 (loopback) | **unauthenticated** — for your MTA only |
| SASL auth | 12325 (loopback) | Postfix `smtpd_sasl_type = dovecot` |
| Quota policy | 12340 (loopback) | Postfix `check_policy_service` |

## 4. TLS

- Local: `./gen-certs.sh <hostname>` writes a self-signed `tls/cert.pem` +
  `tls/key.pem` (key is world-readable so the uid-1000 container can load it).
- Production: drop a real cert/key (e.g. Let's Encrypt `fullchain.pem` /
  `privkey.pem`) into `deploy/compose/tls/` as `cert.pem` / `key.pem` and
  restart the login proxies. `tls/` is git-ignored.

## 5. Fronting MTA (Postfix)

Inbound mail reaches yarilo over **LMTP** (24); outbound is your MTA's job. Wire
Postfix to yarilo:

- Deliver local recipients: `mailbox_transport = lmtp:inet:127.0.0.1:24`.
- SMTP AUTH against yarilo: `smtpd_sasl_type = dovecot`,
  `smtpd_sasl_path = inet:127.0.0.1:12325`.
- Reject over-quota recipients: `check_policy_service inet:127.0.0.1:12340` in
  `smtpd_recipient_restrictions`.

For a public deployment you also need MX / SPF / DKIM (signed at the MTA) / PTR.

## 6. Verifying

- `docker compose ps` — every container `healthy`.
- IMAP/POP3/Submission/ManageSieve greetings via `openssl s_client` / `nc`.
- The smoke test (the entrypoint dispatches on `YARILO_COMPONENT`, so run the
  binary directly):
  ```sh
  docker compose exec yarilo-imap \
    yarilo-smoketest -host yarilo-imap-login -imap-port 10993 \
    -telemetry http://yarilo-imap-login:8080 -insecure=true
  ```

## 7. Operations

- **Logs**: `docker compose logs -f yarilo-imap` (or any service); `LOG_LEVEL=debug`
  in `.env` for verbose output.
- **Upgrade**: bump `YARILO_TAG` in `.env`, then `docker compose pull && docker
  compose up -d`.
- **Backup**: snapshot the `mail` (messages + indexes) and `state` (userdb)
  volumes.
- **Admin CLI**: `docker compose exec yarilo-imap yarctl ...`; migrate
  mailbox formats with `yarilo-migrate`.

## 8. Troubleshooting

| Symptom | Check |
|:---|:---|
| a container is unhealthy | `docker compose logs <svc>`; is `tls/cert.pem` present and readable? |
| client connects but gets no greeting | the matching login proxy AND its backend are up; both share the compose network |
| auth always fails | user row exists with `enabled=1`; password has a `{SCHEME}` prefix |
| 0 messages for a user | the `mail` column driver (maildir/sdbox/mdbox) matches how mail was delivered |
| DNS/`network is unreachable` between services | recreate cleanly: `docker compose down && docker compose up -d` (stale network) |
