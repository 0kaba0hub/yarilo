# JMAP configuration

> **Status: in progress.** The session resource (RFC 8620 §2), the request
> envelope (§3) and the read-only `Mailbox` methods (RFC 8621 §2) are served
> end-to-end; the remaining data methods land in later phases.

JMAP runs as two binaries, not one. `yarilo-jmap-login` faces clients and
`yarilo-jmap` owns the user's state; see
[DEPLOYMENT.md](DEPLOYMENT.md) for the topology and the reasoning behind it.

| Binary | Faces | Terminates client TLS | Runs the passdb chain |
|:---|:---|:---:|:---:|
| `yarilo-jmap-login` | the internet | yes | yes |
| `yarilo-jmap` | the login pod only | no | no |

---

## Listeners

| Service key | Port | Served by | Protocol |
|:---|:---|:---|:---|
| `jmap` | `8443` | `yarilo-jmap-login` | HTTPS, client-facing |
| `jmap_be` | `10443` | `yarilo-jmap` | HTTP over internal mTLS |

`10443` sits in the backend data range next to `10143`/`10110`/`10587`/`10024`
and deliberately clear of `8080`, which is the telemetry port on every
component pod.

---

## Trust between the two

The backend runs no authentication of its own: the login layer already ran the
passdb chain and asserts the user in `X-Yarilo-User`. It therefore honours those
headers only from a peer it has been configured to trust. Three modes, evaluated
in order, all default-deny:

| # | Configure | Anchor | Anything else |
|:--|:---|:---|:---|
| 1 | `internal_tls.enabled: true` | the peer's certificate | `403` |
| 2 | `services.jmap_be.xclient_protocol: true` + `general.xclient.trusted_nets` | the peer's address | `403` |
| 3 | neither | none | `403` on every request |

Mode 2 reuses `general.xclient.trusted_nets`, the same list that already decides
whether a forwarded client IP is believed for XCLIENT and `IMAP ID
x-originating-ip` — `Forwarded` is the HTTP member of that family, not a new
mechanism.

In mode 3 the listener still binds and answers `403` with a named cause, and the
backend logs `no trust anchor for the login hop` once at startup. A dead port
would read as a network fault and send the operator looking in the wrong place.

`allow_nets` is not enforced here: the login pod checks it against the real
client before proxying, exactly as it does for the byte-pipe protocols.

---

## `protocol.jmap`

Every limit is advertised in the session resource as well as enforced, because
clients batch against what is published.

| Key | Default | Description |
|:---|:---|:---|
| `jmap_base_url` | — | Public origin clients reach this deployment on. Prefixes every URL in the session resource, so it must be the externally visible name, not a pod address. |
| `jmap_max_concurrent_requests` | `10` | Simultaneous API calls per session. |
| `jmap_max_calls_in_request` | `16` | Method calls in one batch. |
| `jmap_max_objects_in_get` | `500` | Objects per `Foo/get`. |
| `jmap_max_objects_in_set` | `500` | Objects per `Foo/set`. |
| `jmap_max_size_upload` | `40M` | One blob upload. |
| `jmap_max_size_request` | `10M` | One API request body. |
| `jmap_push_timeout` | `90` | Idle timeout for a push connection, seconds. Unused until the push phase. |
| `jmap_cors_allow_origins` | `[]` | Browser origins allowed to call the endpoint. Empty denies every cross-origin request. Exact match, scheme included. |

The session `state` string is derived from these values, so changing one
invalidates a client's cached session and nothing else does.

---

## Helm

```yaml
components:
  jmap:                    # the backend container
    enabled: true
    listeners:
      jmap:
        containerPort: 10443
        xclient: false     # trust mode 2; leave off when internalTLS is on
    internalTLS:
      enabled: true
      secretName: yarilo-internal-tls

  jmapLogin:               # the client-facing proxy
    enabled: true
    backend_port: 10443    # the JMAP container's port, not the pod's
    director_addr: "yarilo-director:9090"
    tls:
      secretName: jmap-tls

protocol:
  jmap:
    jmap_base_url: "https://mail.example.com"
```

Under the co-located backend model (`components.backend.coLocated: true`, the
director shape) `yarilo-jmap` renders as one more container in the backend pod
and shares its pod IP, so a user reaches every protocol on the address the ring
resolved. With `coLocated: false` (the standalone shape) it renders as its own
Deployment plus a headless Service, like the other backends.

---

## The API endpoint

`POST /jmap/api/` runs a batch of method calls. Two things about it are worth
stating because they are choices, not consequences of the RFC:

**Core is not implicit.** A client must name every capability it relies on in
`using`, including `urn:ietf:params:jmap:core`. A method whose capability is
absent is answered with `unknownMethod` — the same answer as a method that does
not exist (RFC 8620 §3.2). That is what lets the server gain capabilities
without changing how an older client behaves.

**Failures have two levels.** A request-level fault — malformed JSON, a missing
`methodCalls`, an unknown capability, an exceeded request limit — aborts the
batch and returns a problem document. A method-level fault becomes an `error`
response for that one call, and the rest of the batch still runs; a client
matches responses to calls by `callId`, never by position.

**No method executes until the entire envelope is read and parsed; streaming
execution is forbidden.** A request is either rejected whole or run whole, which
is what makes the request-level problems above meaningful. It also fixes the
boundary for the mutating methods that arrive later: a batch cannot be applied
halfway because the client's connection died mid-body.

Back-references (§3.7) resolve against the responses produced so far, so a
reference forward, or into a call answered with `error`, is
`invalidResultReference` by construction. The path is a JSON Pointer (RFC 6901)
extended with `*`, which maps the remainder over an array and flattens one
level.

### Request limits

| Bound | Enforced by | Refusal |
|:---|:---|:---|
| `jmap_max_size_request` | `yarilo-jmap-login`, at the edge | `413` with a `limit` problem naming `maxSizeRequest` |
| `jmap_max_calls_in_request` | `yarilo-jmap` | `400` with a `limit` problem naming `maxCallsInRequest` |

The body cap runs in the login pod so an oversized request is refused before it
is proxied and no backend ever reads it. The backend keeps the same cap as a
floor: a request arriving there oversized means the hop was bypassed. Every
limit problem carries the `limit` member (§3.6.1) — "too big" is useless without
naming the bound.

The clean refusal needs a declared length. A request whose `Content-Length`
exceeds the cap is answered with the `limit` problem before anything is read; a
chunked body, or one that understates its length, is instead cut off by the
reader as it is proxied, so that client sees a broken response rather than a
problem document. The excess never reaches a backend either way, and a client
sending `Content-Length` — which is every real JMAP client — always gets the
parseable answer.

---

## Mailboxes

`Mailbox/get` and `Mailbox/query` are read-only and expose the **personal
namespace** only; shared and public namespaces arrive with the namespace phase.

| JMAP member | Source |
|:---|:---|
| `id` | the folder GUID from the index — stable across a rename, which the name is not |
| `name` | the leaf name; hierarchy travels in `parentId`, so a client never learns the delimiter |
| `role` | IMAP special-use, per-user overrides layered over `imap_special_use_defaults`; `INBOX` is `inbox` without carrying an attribute |
| `totalEmails` / `unreadEmails` | the folder's own counters, the same ones `STATUS` reports |
| `totalThreads` / `unreadThreads` | equal to the message counts: until threading lands each message is its own thread |
| `isSubscribed` | the same subscriptions file IMAP reads, so one `SUBSCRIBE` shows in both protocols |
| `myRights` | full rights in the personal namespace; a `\NoSelect` container reports no read, add, remove or submit |

A `\NoSelect` container appears in the list with a `container:` id. It holds no
mail, but omitting it would leave its children pointing at a parent the client
never saw.

Both methods read control files under the cross-process lock, so a concurrent
IMAP `SUBSCRIBE` or `CREATE (USE ...)` cannot be observed half-applied. The
backend refuses to start without a locks client for that reason.

### State strings

`Mailbox/get` returns `state` and `Mailbox/query` returns `queryState`; both are
a digest of the mailbox set. They tell a client that something moved, never
what. `canCalculateChanges` is `false` and stays false until `Mailbox/changes`
lands, so a client refetches rather than diffing — the same non-incremental
contract `Email/query` will start with.

### What is refused rather than approximated

A filter operator (`AND`/`OR`/`NOT`) is answered with `unsupportedFilter`, and a
sort on any property other than `sortOrder`, `name` or `parentId` with
`unsupportedSort`. Silently matching everything, or returning a different order
than the one asked for, would render the wrong list in a client that had no way
to know.

---

## Capabilities

| Capability | RFC | State |
|:---|:---|:---|
| Core — session | RFC 8620 §2 | served |
| Core — request envelope, back-references, `Core/echo` | RFC 8620 §3–§4 | served |
| Mail — `Mailbox/get`, `Mailbox/query` | RFC 8621 §2 | served, read-only |
| Mail — `Mailbox/set`, `Mailbox/changes` | RFC 8621 §2 | later phase |
| Mail — `Email`, `Thread`, `SearchSnippet` | RFC 8621 §4–§7 | later phase |
| Push over WebSocket | RFC 8887 | later phase |

The protocol layer lives in `pkg/jmapcore`, which imports nothing from yarilo
and is meant to be extracted as a standalone library.

---

## Smoke check

```sh
smoketest -host mail.example.com -jmap -jmap-user u1@example.com -jmap-pass secret
```

Four checks:

1. an anonymous request is refused with `401`;
2. an authenticated one returns a session resource carrying `capabilities`;
3. a batch of two `Core/echo` calls runs, the second reading the first's result
   through a back-reference;
4. a body one byte over `jmap_max_size_request` is refused by the login layer
   with a `limit` problem.

Check 4 sends `-jmap-max-size-request` + 1 bytes; pass the deployment's own
value if it differs from the 10M default.
