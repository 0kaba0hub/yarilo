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

## Load tests

[yarilo-loadtest](https://github.com/yarilomail/yarilo-loadtest) is the load
generator: LMTP delivery and persistent IMAP sessions, with a configurable
corpus. It is separate from the smoke tests, which answer "is it up"; this
answers "what does it cost".

Three Jobs in [`hack/loadtest/`](../hack/loadtest/), each for a different
question:

| Job | Drives | Read alongside |
|:---|:---|:---|
| `lmtp-job.yaml` | delivery, and through it FTS indexing | `fts_build_stage_seconds`, `fts_worker_busy_seconds_total`, `fts_index_queue_depth` |
| `imap-job.yaml` | persistent sessions: append, fetch, store, expunge | per-command percentiles from the run's own summary |
| `search-job.yaml` | SEARCH only, against an index the others filled | search latency without append traffic competing for the same per-user index |

```sh
kubectl apply -f hack/loadtest/lmtp-job.yaml
kubectl -n yarilo-sb logs -f job/yarilo-loadtest-lmtp
```

### Reading the result

The summary has an `errors` column and a `cancel` column. **`errors` means the
server did something wrong; `cancel` counts operations the run cut off when
`-duration` expired** — roughly one per client, so a clean run reports something
like `0 errors, 8 cancelled at the deadline`. Any non-zero `errors` is a
finding.

The live table prints one line per interval rather than a running total,
because a rate that collapses at forty seconds and a rate that was never good
average to the same number.

### Choices in the manifests that are not incidental

**The corpus is 20 KB–1 MB with half the messages carrying an attachment.**
Tokenisation cost scales with body size, so a corpus of small plain-text notes
reports a per-message cost no real deployment sees.

**The seed is fixed.** Two runs against different versions then compare the
server rather than the corpus.

**Recipients span `u1..u150`**, which covers all three mailbox types in the
sandbox — mdbox `u1-50`, maildir `u51-100`, sdbox `u101-150`. A run confined to
one type measures that type.

**`-msgs` is a steady state, not a total.** Without it the mailboxes grow
through the whole run, so the same operation costs more at the end than at the
start. `-msgs=0` turns it off, which is what the search-only Job needs.

**`-mailboxes-per-user=4`** produces the condition worth testing against a
server that dispatches index work per user: more than one mailbox of one user
wanting a pass at the same time.

### Profiling under load

A load run is when the profilers are worth having. With
`telemetry.pprof.enabled` set (see
[DEPLOYMENT.md](DEPLOYMENT.md#profiling-a-live-pod)), start a Job, then:

```sh
kubectl -n yarilo-sb port-forward yarilo-backend-0 8085:8085   # the fts container's telemetry port
go tool pprof -http=: "http://localhost:8085/debug/pprof/profile?seconds=30"
```

Take the profile while the Job is running, not after — an idle process profiles
its idle loop, which is a picture of nothing.
