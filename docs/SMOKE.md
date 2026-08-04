# End-to-end smoke test

Drives a live yarilo instance through the full happy-path mail flow:
authenticate → deliver → read.

| Step | What it exercises |
|:---|:---|
| Submission AUTH PLAIN over STARTTLS | passdb chain, bcrypt verify, STARTTLS handshake |
| Submission AUTH LOGIN over STARTTLS | legacy SASL LOGIN mechanism (Outlook, Android MUAs) |
| LMTP delivery | storage write path, auto-provisioning of new mailboxes |
| IMAPS LOGIN command | IMAP native `LOGIN user password` (RFC 3501) |
| IMAPS AUTHENTICATE PLAIN | IMAP SASL PLAIN via AUTHENTICATE |
| POP3S USER/PASS | POP3 native `USER` + `PASS` |
| POP3S AUTH PLAIN (SASL) | POP3 SASL PLAIN via AUTH (RFC 5034), with initial response |

The harness lives in [`app/smoketest-e2e`](../app/smoketest-e2e) and runs against any yarilo deployment exposing the listeners — local binary, docker compose, or staging cluster.

---

## Quick local run

Generate a self-signed cert + seed a bcrypt-hashed user in SQLite, then start yarilo and run the smoke binary.

```sh
# 1. Workspace
mkdir -p /tmp/yarilo-smoke/{tls,data,mail}
cd /tmp/yarilo-smoke

# 2. Self-signed test certificate (30 days)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout tls/key.pem -out tls/cert.pem -days 30 -nodes \
  -subj "/CN=mail.smoke.local" \
  -addext "subjectAltName=DNS:mail.smoke.local,DNS:localhost,IP:127.0.0.1"

# 3. Seed bcrypt user in SQLite (uses the same passdb code yarilo does)
go run /path/to/yarilo/app/smoketest-e2e/seed/main.go \
  /tmp/yarilo-smoke/data/users.db alice@smoke.local wonderland

# 4. Start yarilo with the smoke config (see below)
go build -o /tmp/yarilo /path/to/yarilo/app/yarilo
/tmp/yarilo -config /tmp/yarilo-smoke/yarilo.yaml &

# 5. Run the smoke
go run /path/to/yarilo/app/smoketest-e2e/ -insecure

# Expected:
# [PASS] submission AUTH PLAIN over STARTTLS
# [PASS] LMTP deliver to mailbox
# [PASS] IMAPS LOGIN + SELECT INBOX + FETCH
# [PASS] POP3S USER/PASS + STAT + RETR
```

`-insecure` accepts the self-signed certificate. Drop the flag when running against staging/prod with a real CA-signed cert.

---

## Smoke config (`/tmp/yarilo-smoke/yarilo.yaml`)

Uses high ports (9000+) to avoid needing root.

```yaml
mode: single
general:
  ssl:
    tls_cert: /tmp/yarilo-smoke/tls/cert.pem
    tls_key:  /tmp/yarilo-smoke/tls/key.pem
  haproxy:
    trusted_nets: ["127.0.0.1/32"]
  xclient:
    trusted_nets: ["127.0.0.1/32"]
  limits:
    mail_max_userip_connections: 0

services:
  imaps:       { enabled: true, port: 9993, ssl_mode: ssl }
  imap:        { enabled: true, port: 9143, ssl_mode: starttls }
  submission:  { enabled: true, port: 9587, ssl_mode: starttls }
  submissions: { enabled: true, port: 9465, ssl_mode: ssl }
  pop3:        { enabled: true, port: 9110, ssl_mode: starttls }
  pop3s:       { enabled: true, port: 9995, ssl_mode: ssl }
  lmtp:        { enabled: true, port: 9024, ssl_mode: "no" }

protocol:
  submission:
    hostname: mail.smoke.local
    max_message_size: 41943040
  lmtp:
    add_received_header: true

auth:
  passdb:
    - driver: sqlite
      dsn: /tmp/yarilo-smoke/data/users.db

storage:
  mailbox: maildir
  maildir_root: /tmp/yarilo-smoke/mail

log:
  level: debug
```

---

## CLI flags

```
-host          target hostname           (default: 127.0.0.1)
-user          mailbox login             (default: alice@smoke.local)
-pass          password                  (default: wonderland)
-submission-port  STARTTLS submission    (default: 9587)
-lmtp-port        plain TCP LMTP         (default: 9024)
-imaps-port       IMAPS                  (default: 9993)
-pop3s-port       POP3S                  (default: 9995)
-insecure         skip TLS verify         (default: true)
-timeout          per-step timeout        (default: 10s)
```

Against a real deployment, point `-host` at the public hostname and use the standard ports `587 / 24 / 993 / 995`, no `-insecure`.

---

## What auto-provisioning means

When LMTP receives a message for a user whose Maildir doesn't exist, it creates `INBOX/{cur,new,tmp}/` and proceeds. This matches the reference's behavior — LMTP is internal, the upstream MTA has already vetted recipients.

If you want strict recipient validation at the LMTP layer (instead of trusting the MTA), file an issue — there's currently no `lmtp_reject_unknown_recipients` knob.

---

## Exit codes

| Code | Meaning |
|:---|:---|
| 0 | All four steps passed. |
| 1 | One or more steps failed (details on stderr). |

## JMAP header forms and property validation

The last JMAP check is the only one that **writes**. It appends its own message
to a folder of its own, `YariloSmoke`, reads it back through every `header:*`
form of RFC 8621 §4.1.3, and removes both afterwards.

It brings its own message because the alternative is depending on whatever
happens to be in the mailbox — which differs per deployment, so a green run
would mean different things in different places.

**The folder rather than INBOX** because the check runs on every rollout, and a
message left behind changes the mailbox every other measurement is taken
against. Cleanup is best effort and **reported when it fails**: leaving the
folder is recoverable, not knowing it was left is not.

It needs `-jmap-user` and `-jmap-pass`, and it is skipped without them.

What it asserts beyond the forms themselves:

- the response carries **only** the requested properties — an unprojected answer
  states `hasAttachment: false` for a message that has one;
- a missing header is answered `null` and is **present** in the object, checked
  separately from its value because in Go a missing key and a null value are
  both `nil`;
- a misspelled property is refused with `invalidArguments` — silence there is
  indistinguishable from a property yarilo has not implemented;
- `headers` lists every field in the order the message carries them.
