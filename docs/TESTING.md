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
| `namespace` | k8s namespace to read `yarilo-smoke-creds` secret from (default `yarilo-sb`) |

Requires GitHub Actions secret `KUBE_CONFIG_SANDBOX` — base64-encoded kubeconfig with read access to the target namespace.

### Create smoke credentials secret

```sh
kubectl create secret generic yarilo-smoke-creds \
  --namespace yarilo-sb \
  --from-literal=user=u1@d00001.test \
  --from-literal=password='Yarilo!test1'
```

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
