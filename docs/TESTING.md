# Testing

## Smoke tests

`app/smoketest` covers: telemetry `/healthz` + `/readyz`, POP3S greeting, ManageSieve, Sieve execution.

IMAP conformance is covered separately by [dovecot/imaptest](https://github.com/dovecot/imaptest).

### Run via GitHub Actions

Trigger `smoke.yml` (`workflow_dispatch`) with:

| Input | Description |
|:---|:---|
| `host` | yarilo hostname, e.g. `mail-sb.seconddns.com` |
| `imap_port` | IMAPS port (default `993`) |
| `pop3s_port` | POP3S port (leave empty to skip) |
| `telemetry_url` | Telemetry base URL, e.g. `http://10.0.0.1:8080` |
| `insecure` | Skip TLS cert verification (`true`/`false`) |

Requires GitHub Actions repository secrets:

| Secret | Value |
|:---|:---|
| `SMOKE_IMAP_USER` | IMAP test account, e.g. `u1@d00001.test` |
| `SMOKE_IMAP_PASS` | IMAP test account password |

### Run imaptest manually against sandbox

```sh
docker run --rm dovecot/imaptest \
  host=mail-sb.seconddns.com \
  port=993 \
  ssl=yes \
  user=u1@d00001.test \
  pass='Yarilo!test1' \
  no_pipelining=yes \
  clients=1 \
  count=5
```
