# Testing

## Smoke tests

There are two smoke binaries, and which one to reach for depends on the
question. They are not alternatives.

| | question it answers | how it is run |
|:---|:---|:---|
| [`app/smoketest`](../app/smoketest) | is *this deployment* serving correctly? | against a live cluster, per rollout — `smoke.yml`, or by hand |
| [`app/smoketest-e2e`](../app/smoketest-e2e) | does mail get in and out at all? | against any yarilo — local binary, compose, or staging |

**`app/smoketest`** is the per-rollout gate: telemetry `/healthz` and `/readyz`,
the POP3S greeting, ManageSieve, Sieve execution, quota and ACL, FTS, and the
JMAP checks including the header forms. It checks a deployment.

**`app/smoketest-e2e`** drives the whole happy path in one pass — Submission
AUTH over STARTTLS, LMTP delivery, then reading the same message back over both
IMAPS and POP3S, with each protocol's native and SASL authentication. It checks
that the parts fit together, which the per-rollout checks do not: each of those
looks at one listener.

It needs a seeded account, and both the seeding and the invocation are in
**[SMOKE.md](SMOKE.md)**, which is also where the step-by-step table lives.

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
| `jmap-job.yaml` | the read chain a client opens with: session, `Mailbox/get`, `Email/query`+`get` | per-method latency, and `nr_throttled` beside it |
| `pop3-job.yaml` | full POP3 sessions: connect, authenticate, survey, retrieve, quit | per-command latency, and the maildrop lock under concurrency |

```sh
kubectl apply -f hack/loadtest/lmtp-job.yaml
kubectl -n yarilo-sb logs -f job/yarilo-loadtest-lmtp
```

**The JMAP job measures the read path and does not check protocol behaviour.**
The driver asks for `id`, `subject`, `from`, `receivedAt` and `threadId`, so
nothing in a load run exercises the `header:*` forms or property validation.
Those want a conformance check; a load run that happened to pass would say
nothing about them.

And for JMAP in particular, read
[the throttling section](#check-for-cpu-throttling-before-believing-a-latency-number)
first. A 5085 ms median once looked like an algorithmic defect and was a 500m
CPU limit.

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

**POP3 opens and closes a session per iteration**, which is the protocol rather
than a shortcut: the server locks the maildrop for the length of a session, so a
generator that held its connections would measure its own lock contention and
report it as the server's. `-delete` stays off — it consumes the mailboxes every
other run measures against.

**`-mailboxes-per-user=4`** produces the condition worth testing against a
server that dispatches index work per user: more than one mailbox of one user
wanting a pass at the same time.

### Check for CPU throttling before believing a latency number

**Do this first, every time.** A container at its CPU limit produces latencies
that look exactly like a defect in the code, and no amount of profiling the code
will find them — the profile shows the work spread over wall-clock time that the
scheduler took away.

This is not hypothetical. JMAP read latency was measured at 5085 ms median and
investigated as an algorithmic problem, complete with a benchmark and three
proposed redesigns. The container had `limits.cpu: 500m` and was consuming ~450m
of it:

```
nr_periods 2226 · nr_throttled 1806 · throttled_usec 286912352
```

81% of scheduler periods stopped. Raising the limit and changing nothing else:

| | `cpu: 500m` | `cpu: 3` |
|:---|---:|---:|
| `Email/query`+`get` median | 5085 ms | **322.9 ms** |
| `Mailbox/get` median | 1103 ms | 132.2 ms |
| throughput | 1.2 ops/s | 15.3 ops/s |
| `nr_throttled` | 1806 / 2226 | 0 / 840 |

A fifteen-fold difference from a value in `values.yaml`.

Read it from inside the container — cgroup v2 exposes it directly:

```sh
kubectl -n <ns> exec <pod> -c <container> -- cat /sys/fs/cgroup/cpu.stat
```

`nr_throttled` against `nr_periods` is the whole answer. Anything above a few
percent means the numbers beside it describe the quota, not the server.

The kubelet also exports `container_cpu_cfs_throttled_periods_total` per
container via cAdvisor:

```sh
kubectl get --raw "/api/v1/nodes/<node>/proxy/metrics/cadvisor" \
  | grep container_cpu_cfs_throttled_periods_total
```

**But nothing scrapes it.** The cluster runs metrics-server only, with no
Prometheus, so this is available on demand and not in history — there is no
answering "was it throttled during yesterday's run" after the fact. Take the
reading while the run is going, or record it with the result.

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

And read `cpu.stat` in the same window. A profile taken through throttling
attributes the work correctly and the *time* misleadingly: the shares are real,
the seconds are the quota's.
