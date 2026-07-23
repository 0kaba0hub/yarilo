# Yarilo — Internal Protocol Specification

All internal protocols are derived from Dovecot 2.3.21 source analysis.
Transport: UNIX socket or TCP. Format: TAB-delimited text, LF-terminated lines.
TAB escaping: `\t` → `\011`, `\\` → `\\`, `\n` → `\012`, `\0` → `\000`.

---

## Common Rules

**Version handshake** (every protocol):
```
VERSION\t<name>\t<major>\t<minor>\n
```
- Major must match. Minor may differ (backward compatible).
- Connection rejected if major mismatch.

**Line length limit**: 16384 bytes (all protocols).

**Escaping**: `str_tabescape` / `str_tabunescape` — same as Dovecot strescape.

---

## 1. yarilo-director — Director Ring Protocol

**Version**: `yarilo-director 1 0`
**Transport**: TCP port 9091 (configurable)
**Topology**: Circular ring — each director has one left (incoming) and one right (outgoing) connection.

### Consistent Hashing

```
username_hash = MD5(username)[0:4]  // first 32 bits of MD5
vhost_hash    = MD5(host_ip + "-" + i)[0:4]  // 100 vhosts per backend (i=0..99)
```

Ring lookup: binary search on sorted vhost array → wrap to first if hash > last entry.

Backend weight = vhost_count (default 100). Down backends: vhost_count = 0.

### User TTL

```
user_expire          = 900s (default, configurable)
user_near_expiring   = clamp(10% of user_expire, 3s .. 30s)
```

Users stored as LRU list sorted by timestamp. Expired users removed by background timer.

### Outgoing Handshake (connection initiator sends):

```
VERSION\tyarilo-director\t1\t0\n
ME\t<ip>\t<port>\t<timestamp>\n
DIRECTOR\t<ip>\t<port>\n          (repeat for each known director)
HOST-HAND-START\t<ring_completed>\n
HOST\t<ip>\t<vhost_count>\t<tag>\t<D|U><timestamp>\t<hostname>\n  (repeat)
HOST-HAND-END\t<ring_completed>\n
USER\t<hash32>\t<backend_ip>\t<timestamp>\n   (repeat for each known user, v9+)
OPTIONS\tconsistent-hashing\n
DONE\n
```

### Incoming Handshake (receiver sends):

```
VERSION\tyarilo-director\t1\t0\n
ME\t<ip>\t<port>\t<timestamp>\n
DONE\n
```

### Commands (post-handshake):

**Host management:**
```
HOST\t<ip>\t<vhost_count>\t<tag>\t<D|U><timestamp>\t<hostname>\n
HOST-REMOVE\t<ip>\t<port>\n
```

**User management:**
```
USER\t<hash32>\t<backend_ip>\t<timestamp>\n
USER-WEAK\t<origin_ip>\t<origin_port>\t<seq>\t<hash32>\t<backend_ip>\n
USER-MOVE\t<origin_ip>\t<origin_port>\t<seq>\t<hash32>\t<backend_ip>\n
USER-KICK\t<origin_ip>\t<origin_port>\t<seq>\t<username>\n
USER-KICK-HASH\t<origin_ip>\t<origin_port>\t<seq>\t<hash32>\t<except_ip>\n
USER-KILLED\t<hash32>\n
USER-KILLED-EVERYWHERE\t<origin_ip>\t<origin_port>\t<seq>\t<hash32>\n
```

**Ring sync:**
```
SYNC\t<origin_ip>\t<origin_port>\t<seq>\t<minor>\t<timestamp>\t<hosts_hash>\n
DIRECTOR\t<ip>\t<port>\n
DIRECTOR-REMOVE\t<ip>\t<port>\n
CONNECT\t<ip>\t<port>\n
```

**Keepalive:**
```
PING\t<timestamp>\t<buffer_size>\n
PONG\t<timestamp>\t<buffer_size>\n
QUIT\t<reason>\n
```

### Timeouts:
```
connect_timeout       = 10s
me_timeout            = 10s
send_users_timeout    = 30s
done_timeout          = 40s
ping_interval         = 15s
ping_sync_interval    = 1s   (when syncing)
pingpong_warn         = 5s
request_timeout       = 30s
reconnect_retry       = 60s
```

### yarilo's actual ring handshake (self-organizing ring, #750 — supersedes the earlier full-mesh PeerDialer, #700)

`internal/director/membership.go` implements a simplified subset of the
above (no `DIRECTOR`/`USER`/`OPTIONS`/`SYNC` lines): members are ordered by
`(ip, port)`, and — unlike the reference's mesh — each node dials only its
**right neighbor** in that sorted order (wrapping around), never every peer.
Routing truth stays the deterministic ring hash (`pkg/cluster/ring`);
membership exists purely to redundantly share routing state between
neighbors, not to elect or vote.

Degradation ladder (every member count is a fully valid, service-serving
state — never refuses service):

```
N=1: no neighbors, no ring connections at all — ordinary single-replica mode.
N=2: left == right; exactly ONE connection serves both directions — only
     the lexicographically lower (ip,port) member dials; the higher member
     never dials, but that one shared connection carries traffic both ways.
N=3+: every member dials its right neighbor — N distinct directed edges,
      a proper cycle.
```

**Join** (a fresh connection to a seed — normally the director Service's
stable ClusterIP, sometimes a manually configured peer address for non-k8s
— separate from, and closed before, the actual ring data connection):
```
Client → Server:  DIRECTOR-JOIN\t<ip>\t<port>\n
Server → Client:  JOIN-CHALLENGE\t<nonce_hex>\n   or   JOIN-FAIL\t<reason>\n
Client → Server:  JOIN-PROOF\t<hmac_hex>\n         (HMAC-SHA256(ring_secret, nonce+"\t"+ip+"\t"+port))
Server → Client:  JOIN-OK\n
                  DIRECTOR-LIST\t<ip1>:<port1>,...\t<removed_ip1>:<removed_port1>,...\n
                                        (existing members + tombstoned/dead members, #754; joiner adds itself)
                  DONE\n
              or: JOIN-FAIL\t<reason>\n
```
An empty/unconfigured `ring_secret` on the acceptor rejects every JOIN
outright (`ring auth not configured`) — that node can then only ever run as
a singleton. On success the acceptor also propagates `DIRECTOR-ADD` around
the ring and recomputes its own right neighbor; the joiner separately dials
its own computed right neighbor as an ordinary ring connection.

**Ring data connection** (right-neighbor dial — same VERSION/ME/PEER/DONE
handshake #700 introduced, now targeting one computed neighbor instead of
every configured peer):
```
VERSION\tyarilo-director\t1\t0\n
ME\t<ip>\t<port>\t<timestamp>\n
MEMBERS\t<ip1>:<port1>,...\t<removed_ip1>:<removed_port1>,...\n
PEER\t1\n
DONE\n
```
`PEER\t1` still marks the connection as a ring/replica connection rather
than a login proxy (`client.isPeer`) — a login proxy's generic
`cluster/proto` dialer never sends it.

`MEMBERS` (#754) is sent by the dialer *before* `PEER` deliberately: the
acceptor's CONNECT-redirect decision (triggered by the `PEER` line, see
below) uses its own membership view, and without this the acceptor could
redirect the dialer straight back toward a member the dialer already knows
is dead — precisely because it's dialing elsewhere to route around that
death — and the redirect path never reaches the post-handshake
`DIRECTOR-LIST` resync that would otherwise fix the acceptor's stale view.
Merging the dialer's tombstones first closes that gap regardless of which
way the connection ends up being used.

Immediately after the handshake, **both** ends additionally send a
`DIRECTOR-LIST\t<members-csv>\t<removed-csv>\n` snapshot of their current
member list *and* tombstone set (each side's set is unioned into the
other's — including tombstones, never a blind replace) — this closes a
same-process race where a `DIRECTOR-ADD`/`DIRECTOR-REMOVE` fired between a
membership change being accepted and that node's own (possibly
just-retargeted) dial completing would otherwise vanish, without needing
the full user/backend state snapshot planned for #750 phase 3.

**Tombstones** (#754): `removed` is a permanent per-node set of members
known to be dead — required because a plain union-merge of member lists
alone would let any peer whose view hasn't caught up yet silently
resurrect a member some other node already correctly evicted, on every
reconnect. `addMember` (used both when accepting a fresh authenticated
JOIN and when applying a relayed `DIRECTOR-ADD`) clears the tombstone for
that `(ip, port)` — trusted to mean the address is alive again, whether
that's a genuine rejoin or the address was reassigned to a new pod.

A connection may also receive, at any point: `CONNECT\t<ip>\t<port>\n` —
sent by an acceptor that determines the dialer picked a stale target (its
membership view says someone else should be receiving this dial); the
dialer retries immediately against the given address instead of treating
it as a failure. Because a redirect can change *who the dialer is actually
trying to reach* mid-attempt, death-declaration after exhausting retries
must track and blame that current address, never the originally intended
one — conflating the two was the direct cause of a live member being
wrongly declared dead in #754's regression (a stale redirect bounced the
dialer toward an already-dead member; exhausting retries against it then
mis-attributed the death to the node it had originally been trying to
reach instead).

**Event forwarding** (RING-CHANGE / USER-MOVED / USER-KICKED / DIRECTOR-ADD
/ DIRECTOR-REMOVE) replaces #700's full-mesh direct-broadcast-to-every-peer
with proper ring propagation, each line carrying an origin + sequence
envelope:
```
<KIND>\t<originIP>\t<originPort>\t<seq>\t<...original payload>\n
```
A node receiving one of these: if `origin` is itself, the event has
travelled all the way around the ring and is **absorbed** (never
re-applied, never re-forwarded — this is what makes the ring self-limiting
with zero coordination, and at N=2 is what turns "forward unconditionally"
into exactly one round trip rather than an infinite bounce, since the
single shared connection there serves as both "left" and "right"). Otherwise
it applies the change locally (unless `seq` is not newer than the highest
already seen from that origin — a safety net against duplicates) and
forwards unconditionally to its own right neighbor. Local login clients
never see the envelope form — they keep receiving the plain, historical
`<KIND>\t<...payload>\n` line unchanged.

**Death detection** in phase 1 is read-error-based, not timeout-based: a
node whose right-neighbor dial fails (after a few short retries) or drops
declares that member dead, removes it locally, announces `DIRECTOR-REMOVE`
around the ring, and recomputes its own right neighbor — naturally skipping
the dead member to whoever's next. An *accepted* connection dropping never
triggers a death declaration; that is always the dialing side's
responsibility. Faster, actively-probed ring timeouts (rather than relying
on the OS/TCP to eventually surface a dead connection) are planned for
#750 phase 4 alongside `members_hash`-based anti-entropy.

### USER-WEAK Flow (sticky session re-sync):
1. User nearing TTL expiry → send `USER-WEAK` through ring
2. All directors mark user weak, forward message
3. Message returns to origin → send definitive `USER` (weak=false)
4. No new connections allowed while user is weak

### Sequence Deduplication:
Each director tracks `last_seq` per origin. Commands with `seq <= last_seq` are silently dropped (prevents ring loops).

### Proxy → Director Query (gRPC sidecar or UNIX socket):
```
LOOKUP\t<username>\t<tag>\n
→ HOST\t<backend_ip>\t<backend_port>\t<hostname>\n
→ NOTFOUND\n
→ FAIL\t<reason>\n
```

### yarilo's actual LOOKUP tag semantics (#737)

The wire format is `LOOKUP\t{id}\t{user}\t{tag}` (`internal/cluster/proto`) —
the tag field is **mandatory**, always sent by every client, including when
a login pod has no tag configured. There is no "full ring, ignore all tags"
mode in either the reference implementation or yarilo: `""` means the
*untagged pool* (backends with no tag set), not "any tag." A login pod's
`director_tag` config (or `login.Options.Tag` / `lmtplogin.Options.DirectorTag`
in-process) must match the tag of the backend pool it serves — per
`DEPLOYMENT.md`'s tag-based sharding model, one login Deployment maps to
exactly one tag-pool. In a deployment with no tags configured at all, every
backend has tag `""`, so `LOOKUP` with `tag=""` is equivalent to routing
over the full ring — this is why untagged/standalone deployments see no
behavior change.

---

## 2. yarilo-auth — Authentication Protocol

**Version**: `yarilo-auth 1 0`
**Transport**: UNIX socket `$run_dir/auth`
**Line limit**: 16384 bytes
**Cookie size**: 16 bytes (128 bits)

### Server → Client Handshake:
```
VERSION\tyarilo-auth\t1\t0\n
MECH\t<name>\t<flags>\n    (one per supported SASL mechanism)
SPID\t<server_pid>\n
CUID\t<connect_uid>\n
COOKIE\t<32_hex_chars>\n
DONE\n
```

### Client → Server:
```
CPID\t<client_pid>\n
AUTH\t<id>\t<mechanism>\tservice=<service>\t[secured]\t[tls_cipher=<c>]\t[resp=<base64>]\t[lip=<ip>]\t[rip=<ip>]\t[lport=<port>]\t[rport=<port>]\t[session=<id>]\n
CONT\t<id>\t<base64_response>\n
CANCEL\t<id>\n
VERIFY\t<id>\t<token>\n
```

Notes:
- `rip=<ip>` — real mail-client IP forwarded by the login pod. Used for penalty tracking instead of the TCP-peer (login pod) IP.
- `session=<id>` — anvil session ID generated by the login pod. Stored in the issued token so the backend can verify consistency.
- `VERIFY` — sent by backend pods (not login pods) to consume a one-time token issued in the OK response and enter authenticated state without re-running passdb.

### Server → Client Responses:
```
OK\t<id>\tuser=<username>\t[home=<dir>]\t[mail=<loc>]\t[uid=<n>]\t[gid=<n>]\t[proxy]\t[proxy_maybe]\t[host=<h>]\t[port=<p>]\t[destuser=<u>]\t[pass=<p>]\t[nologin]\t[allow_nets=<cidr,…>]\t[director_tag=<tag>]\t[token=<64hex>]\n
FAIL\t<id>\t[temp_fail]\t[authz_fail]\t[user_disabled]\t[pass_expired]\t[reason=<str>]\n
CONT\t<id>\t<base64_challenge>\n
```

VERIFY responses:
```
OK\t<id>\tuser=<username>\tsession=<sessionID>\n
FAIL\t<id>\t[reason=not-configured|reason=bad-request]\n
```

- `token=<64hex>` — present in OK when `auth.token.ttl_seconds > 0` AND `session=<id>` was supplied in the AUTH request. 32-byte random value (hex-encoded), valid for one VERIFY call within the configured TTL.
- `allow_nets=<cidr,…>` — comma-separated CIDR/IP list from passdb. The login pod enforces this before forwarding to the backend.
- `director_tag=<tag>` (#746) — per-user director backend tag, sourced from a passdb or userdb `director_tag` extra field (SQL: an ordinary column in `password_query` or `user_query` — no driver code change needed, same generic column-forwarding mechanism as `allow_nets`). When present, the login pod's `directorLookup` uses this tag instead of its static `director_tag` config (#737) for that one user's `LOOKUP`. Lets a single shared login fleet route different users to different tag-pools, as opposed to the static model of one dedicated login Deployment per tag-pool. Absent means no per-user override.

### passdb Chain:
```
PASSDB_RESULT_OK          → stop chain, return OK
PASSDB_RESULT_NEXT        → try next passdb in chain
PASSDB_RESULT_USER_UNKNOWN → try next passdb (if no more → FAIL)
PASSDB_RESULT_PASSWORD_MISMATCH → FAIL immediately
PASSDB_RESULT_INTERNAL_FAILURE → FAIL with temp_fail
```

### Auth Worker Protocol (for blocking passdb: sql, pam):
```
VERSION\tyarilo-auth-worker\t1\t0\n
DBHASH\t<passdb_md5>\t<userdb_md5>\n
---
<id>\t<serialized_auth_request>\n
→ <id>\tOK\t[fields]\n
→ <id>\tFAIL\t[error]\n
→ *\t<continuation>\n   (multi-line, ends with non-* line)
```

Worker idle timeout: 5 minutes. Query timeout: 60s. Queue abort: 60s.

### Master Auth Socket (mail process → auth):
Binary structure (after VERSION handshake):
```c
struct yarilo_auth_request {
    uint32_t tag;
    pid_t    auth_pid;
    uint32_t auth_id;
    uint32_t client_pid;
    uint8_t  cookie[16];
    // ip_addr local_ip, remote_ip
    // uint16_t local_port, remote_port
    uint32_t flags;
    uint32_t data_size;
};

struct yarilo_auth_reply {
    uint32_t tag;
    uint32_t status;   // 0=ok, 1=internal_error
    pid_t    mail_pid;
};
```

### Auth Cache:
- Hash table + LRU doubly-linked list
- Cache key: `{username}\t{passdb_args}` (tab-escaped)
- TTL: `auth_cache_ttl` (positive) + `auth_cache_negative_ttl` (negative hits)
- Cache node: `{ time_t created; bool last_success; char data[]; }`
- Cleared on: SIGHUP, explicit cache_clear RPC

### SASL Mechanisms:
```
PLAIN          — \0authenid\0passwd
LOGIN          — 2-step (Username: / Password:)
SCRAM-SHA-256  — RFC 5802
SCRAM-SHA-1    — RFC 5802
XOAUTH2        — OAuth2 token
ANONYMOUS      — no auth
EXTERNAL       — certificate
```

---

## 3. yarilo-dict — Key-Value Protocol

**Version**: `yarilo-dict 3 2`
**Transport**: UNIX socket `$run_dir/dict`

### Commands (single-char opcode):
```
H<major>\t<minor>\t<value_type>\t<user>\t<dict_uri>\n    HELLO
L<key>\t[<username>]\n                                    LOOKUP
I<flags>\t<max_rows>\t<path>\t[<username>]\n              ITERATE
B<id>\t[<username>]\n                                     BEGIN
C<id>\n                                                   COMMIT
D<id>\n                                                   COMMIT_ASYNC
R<id>\n                                                   ROLLBACK
S<id>\t<key>\t<value>\n                                   SET
U<id>\t<key>\n                                            UNSET
A<id>\t<key>\t<diff>\n                                    ATOMIC_INC
T<id>\t<secs>\t<nsecs>\n                                  TIMESTAMP
```

### Replies:
```
O<value>\n             OK (with value)
M<v1>\t<v2>\t...\n     MULTI_OK (multiple values, v3.2+)
N\n                    NOTFOUND
F<error>\n             FAIL
W<error>\n             WRITE_UNCERTAIN
A<async_id>\n          ASYNC commit ID assigned
+<async_id>\t<reply>\n ASYNC reply received
\n                     ITERATE finished
```

### ITERATE Flags:
```
0x01  RECURSE          recurse into sub-keys
0x02  SORT_BY_KEY
0x04  SORT_BY_VALUE
0x08  NO_VALUE         keys only
0x10  EXACT_KEY
```

### Quota Key Paths:
```
priv/quota/storage    → used bytes (uint64, decimal string)
priv/quota/messages   → used count (uint64, decimal string)
```

### ACL Key Paths:
```
shared/acl/<mailbox>/<identifier>   → rights string (e.g. "lrwstei")
```

### Sieve Script Key Paths:
```
priv/sieve/scripts/<name>           → script content
priv/sieve/active                   → active script name
```

### Backends:
```
sqlite    — default, file-based
mysql     — SQL via database/sql
postgres  — SQL via database/sql
redis     — RESP protocol, AUTH/SELECT/MULTI/EXEC
```

---

## 4. yarilo-admin — Admin Command Protocol

**Version**: `yarilo-admin 1 2`
**Transport**: UNIX socket or TCP (127.0.0.1:9092)

### Handshake:
```
Server: VERSION\tyarilo-admin\t1\t2\n
Server: +\n                              (pre-authenticated via UNIX socket)
   or:
Server: -\n                              (authentication required)
Client: PLAIN\t<base64(\0admin\0password)>\n
Server: +\n
```

### Command Format:
```
<flags>\t<username>\t<command>\t[<arg1>\t<arg2>\t...]\n
```
Flags: `D` = debug, `v` = verbose.

### Response Format:
```
<field1>\t<field2>\t...\n    (data rows, TAB-separated)
...
\n+\n                        (success)
\n-\t<exit_code>\n           (failure)
```

### Multiplex Streaming (v1.1+):
```
Channel format: <channel_byte><length_4bytes><payload>
Channel 'L' (0x4C): log output
Channel '\0' (0x00): main output
```

### Commands:
```
domain list
domain add     <name>
domain delete  <name>

user list      [<domain>]
user add       <email> <password>
user delete    <email>
user passwd    <email> <newpass>
user info      <email>

mailbox list   <user>
mailbox delete <user> <mailbox>
mailbox status <user> <mailbox>

quota get      <user>
quota recalc   <user>

acl get        <user> <mailbox>
acl set        <user> <mailbox> <identifier> <rights>
acl delete     <user> <mailbox> <identifier>

sieve list     <user>
sieve get      <user> <name>
sieve put      <user> <name>
sieve activate <user> <name>
sieve delete   <user> <name>

director status
director hosts
director users [<tag>]
director kick  <user>
director flush <host>

stats dump     [session|user|domain|ip]
reload
```

---

## 5. yarilo-stats — Telemetry Protocol

**Version**: `yarilo-stats 1 0`
**Transport**: UNIX socket `$run_dir/stats`
**Export**: Prometheus `/metrics` on telemetry port (every instance)

### Event Format (service → stats collector):
```
E\t<event_id>\t<parent_id>\t<log_type>\t<name>\t<k1>=<v1>\t...\n   one-shot
B\t<event_id>\t<parent_id>\t<log_type>\t<name>\t<k1>=<v1>\t...\n   begin
U\t<event_id>\t<parent_id>\t<k1>=<v1>\t...\n                        update
F\t<event_id>\n                                                      finish
```

### Metrics Collected:
```
imap_connections_active     gauge
imap_commands_total         counter  {command}
imap_command_duration_secs  histogram {command}
pop3_connections_active     gauge
smtp_connections_active     gauge
lmtp_deliveries_total       counter  {result}
delivery_duration_secs      histogram
auth_attempts_total         counter  {result, mechanism}
storage_operations_total    counter  {op, backend}
director_users_total        gauge    {tag}
director_hosts_total        gauge    {tag, state}
```

---

## 6. Proxy Forwarding Protocols

### IMAP → Backend (ID extension, RFC 2971):
```
C1 ID ("x-originating-ip" "<client_ip>"
        "x-originating-port" "<client_port>"
        "x-connected-ip" "<proxy_ip>"
        "x-connected-port" "<proxy_port>"
        "x-session-id" "<session_id>"
        "x-proxy-ttl" "<ttl-1>"
        "x-forward-<key>" "<value>")
```
- `x-proxy-ttl` starts at `LOGIN_PROXY_TTL = 5`, decremented at each hop
- `x-forward-*` fields: from passdb `forward_` fields (base64 inner encoding)
- Backend must be in `xclient_trusted_nets` to accept without re-auth

### POP3 → Backend (XCLIENT):
```
XCLIENT ADDR=<client_ip> PORT=<client_port> SESSION=<session_id> TTL=<ttl-1> [FORWARD=<base64>]\r\n
```
- FORWARD: base64(tab-separated "key=value\tkey=value") of passdb forward_ fields
- Line limit: 1024 bytes
- Backend checks XCLIENT against trusted_nets

### SMTP/LMTP → Backend (XCLIENT, Postfix-compatible):
```
XCLIENT ADDR=<ip> PORT=<port> HELO=<helo> LOGIN=<user> PROTO=<SMTP|ESMTP|LMTP> SESSION=<id> TTL=<ttl-1>\r\n
```
- Backend replies `220 ` and resets state
- Cannot be sent mid-transaction (after MAIL FROM)

### JMAP → Backend (HTTP headers):
```
X-Forwarded-For: <client_ip>
X-Forwarded-Port: <client_port>
X-Session-ID: <session_id>
X-Proxy-TTL: <ttl-1>
Forwarded: for=<client_ip>;proto=https  (RFC 7239)
```

---

## 7. Index File Formats

### .index (Main Index)
```
Header:
  magic: [version_major=7, version_minor=3, base_header_size, header_size, record_size]
  compat_flags: 0x01 = little-endian
  indexid: uint32 (unique, initially UNIX timestamp)
  flags: HDR_FLAG_CORRUPTED | HAVE_DIRTY | FSCKD
  uid_validity: uint32
  next_uid: uint32 (monotonically increasing)
  messages_count, seen_messages_count, deleted_messages_count: uint32
  first_recent_uid, first_unseen_uid_lowwater, first_deleted_uid_lowwater: uint32
  log_file_seq, log_file_tail_offset, log_file_head_offset: uint32

Record (base, 5 bytes):
  uid:   uint32
  flags: uint8   (ANSWERED=0x01 FLAGGED=0x02 DELETED=0x04 SEEN=0x08 DRAFT=0x10)

Record extensions (variable):
  keywords: bitmask of keyword indexes
  modseq:   uint64 per-message modification sequence
  cache:    uint32 offset into .index.cache file
  vsize:    uint32 per-message virtual (RFC822) size   (record_size=4, align=4)

Header extension "hdr-vsize" (16 bytes, align 8) — aggregate quota cache:
  vsize:         uint64  sum of every message's virtual size
  highest_uid:   uint32  largest UID folded into vsize
  message_count: uint32  messages folded into vsize
  Validity: trusted while {highest_uid, message_count} match the folder;
  otherwise recalculated from the per-record vsize extension (falling back
  to the physical .names size for records predating the extension).
```

### .index.log (Transaction Log)
```
Header:
  magic: [major=1, minor=3, hdr_size]
  indexid: must match main index
  file_seq: uint32 (incremented on rotation, never reused)
  prev_file_seq, prev_file_offset: link to previous log
  create_stamp: UNIX timestamp
  initial_modseq: uint64

Transaction record:
  size: uint32 (total including header)
  type: uint32 (EXPUNGE|APPEND|FLAG_UPDATE|HEADER_UPDATE|EXT_INTRO|...)

Types:
  EXPUNGE       0x00000001  uid ranges (+ EXPUNGE_PROT 0x0000cd90)
  APPEND        0x00000002  mail_index_record[]
  FLAG_UPDATE   0x00000004  add/remove flags for uid ranges
  HEADER_UPDATE 0x00000020  update index header fields
  EXT_INTRO     0x00000040  introduce extension
  EXT_RESET     0x00000080  reset extension with new reset_id
  EXT_HDR_UPDATE 0x00000100 update extension header
  EXT_REC_UPDATE 0x00000200 update extension record data
  KEYWORD_UPDATE 0x00000400 add/remove keyword
  KEYWORD_RESET  0x00000800 clear all keywords
  MODSEQ_UPDATE  0x00008000 update modseq
  BOUNDARY       0x00080000 transaction boundary
  ATTRIBUTE_UPDATE 0x00100000 mailbox attributes
```

### .index.cache (Cache File)
```
Header:
  magic: [major=1, minor=1]
  indexid: must match
  file_seq: uint32

Field decisions:
  NO    (0x00) — not cached
  TEMP  (0x01) — cached for new messages only, dropped after ~1 week
  YES   (0x02) — always cached
  FORCED (0x80) — never auto-change

Cache record:
  prev_offset: uint32  (linked list of records for same message)
  size: uint32
  fields: [{field_id: uint32, [size: uint32,] data}...]
```

---

## 8. Mailbox Format Details

### Maildir Filename:
```
{secs}.M{usecs}P{pid}.{hostname}:2,{flags}

Standard flags (sorted, uppercase):
  D = Draft      (MAIL_DRAFT)
  F = Flagged    (MAIL_FLAGGED)
  R = Replied    (MAIL_ANSWERED)
  S = Seen       (MAIL_SEEN)
  T = Trashed    (MAIL_DELETED)

Keywords: lowercase a-z (max 26), mapped via dovecot-keywords file

Optional size extensions (avoid stat):
  ,S=<physical_size>
  ,W=<virtual_size_crlf>
```

### dovecot-uidlist (version 3):
```
Header: 3 V<uidvalidity> N<nextuid> G<guid128hex>
Lines:  <uid> [S<size>] [W<vsize>] [G<guid>] [P<pop3uidl>] :<filename>
```

### dbox File Format (sdbox/mdbox):
```
Magic bytes:
  PRE:  \001\002         (DBOX_MAGIC_PRE, 2 bytes)
  POST: \n\001\003\n     (DBOX_MAGIC_POST, 4 bytes)
  Version: 2

Message header (fixed):
  \001\002 N <space> <uid_hex_8> <space> <size_hex_16> \n

Metadata block (after message body):
  \n\001\003\n
  <KEY><value>\n  ...
  \n

Metadata keys:
  G = message GUID (128-bit hex)
  R = received timestamp (hex)
  Z = physical size (hex)
  V = virtual size with CRLF (hex)
  P = POP3 UIDL override
  O = POP3 ordering number
  B = original mailbox (recovery)
  X = external attachments: <offset> <size> <flags> <ref>
```

### mdbox Map Index:
```
Header extension:
  highest_file_id: uint32
  rebuild_count:   uint32

Map record per message:
  file_id: uint32   (which m.<file_id> file)
  offset:  uint32   (byte offset in file)
  size:    uint32   (message size incl. metadata)

Per-mailbox record:
  map_uid:   uint32  (UID in global map)
  save_date: uint32  (delivery timestamp)
```

### sdbox File Naming:
```
u.<uid>    (SDBOX_MAIL_FILE_FORMAT)
```

### mdbox File Naming:
```
m.<file_id>    (MDBOX_MAIL_FILE_FORMAT)
```

---

## 9. ACL Rights

### Rights Letters (dovecot-acl file):
```
l = lookup          (see in list, allow subscribe)
r = read            (open mailbox for reading)
w = write           (change flags except seen/deleted)
s = write-seen      (change \Seen flag)
t = write-deleted   (change \Deleted flag)
i = insert          (APPEND / COPY into)
p = post            (Sieve fileinto)
e = expunge         (permanent delete)
k = create          (create child mailboxes)
x = delete          (delete this mailbox)
a = admin           (change ACL)
```

### dovecot-acl File Format:
```
user=alice lrwsteipekxa
user=bob   r
group=staff lrsteip
-authenticated -p
anyone l
```
- Leading `-` = negative right (deny)
- `anyone` = unauthenticated
- `authenticated` = any authenticated user
- `owner` = mailbox owner
- `group-override=<name>` = group with priority override

### ACL Identifier Priority (highest → lowest):
1. `group-override`
2. `user`
3. `group`
4. `authenticated` / `owner`
5. `anyone`

### Cache TTL: 30 seconds (vfile backend).

---

## 10. Quota

### Quota Rule Syntax:
```
quota_rule = <mailbox_mask>:<limits>

<mailbox_mask>: *, INBOX, Trash, Trash:*, etc.
<limits>:
  bytes_limit   — absolute: 1G, 500M, 0 (unlimited)
  count_limit   — message count: 10000
  +20M          — relative: add 20MB to default
  80%           — relative: 80% of default limit
  ignore        — don't count this mailbox

examples:
  quota_rule = *:storage=1G
  quota_rule2 = Trash:+20M
  quota_rule3 = Spam:ignore
  quota_warning = 95% /usr/lib/yarilo/quota-warning.sh 95
  quota_warning2 = -10% /usr/lib/yarilo/quota-warning.sh 85
```

### Grace Period:
```
quota_grace = 10M    — allow 10MB overage for last message delivery
```
Prevents mid-transaction bounces. Resets to hard limit after first overage delivery.

### Check Points:
1. LMTP RCPT phase (if `lmtp_rcpt_check_quota = yes`)
2. IMAP APPEND
3. IMAP COPY (destination quota)
4. Mail delivery via lib-lda

### Dict Quota Paths:
```
priv/quota/storage    → bytes used (decimal uint64)
priv/quota/messages   → message count (decimal uint64)
```

---

## 11. Proxy Loop Prevention

```
LOGIN_PROXY_TTL = 5

Each proxy hop decrements TTL by 1.
If TTL ≤ 1 on arrival → reject with "Too many proxy hops".

Forwarded as:
  IMAP:  "x-proxy-ttl" "<n>"   in ID command
  POP3:  TTL=<n>               in XCLIENT
  SMTP:  TTL=<n>               in XCLIENT
  JMAP:  X-Proxy-TTL: <n>      HTTP header
```

---

## 12. yarilo-anvil — Connection Rate Limiting

**Version**: `anvil 1 0`
**Transport**: UNIX socket `{base_dir}/anvil`
**Purpose**: Per-identity connection counting and brute-force penalty tracking.

### Commands

```
CONNECT\t<pid>\t<ident>\n
DISCONNECT\t<pid>\t<ident>\n
LOOKUP\t<ident>\n                    → <count>\n
CONNECT-DUMP\n                       → <ident>\t<pid>\t<refcount>\n ... \n
PENALTY-GET\t<ident>\n               → <penalty> <timestamp>\n
PENALTY-INC\t<ident>\t<crc32>\t<value>\n
PENALTY-SET-EXPIRE-SECS\t<secs>\n
PENALTY-DUMP\n                       → <ident>\t<penalty>\t<last_penalty_ts>\t<last_update_ts>\n ... \n
```

`<ident>` format: `<service>/<ip>/<username>` e.g. `imap/192.168.1.1/alice`

### Penalty Algorithm

- Penalty: 16-bit value (0–65535), decays after `expire_secs` (default 3600s).
- `<crc32>`: CRC32 of username+password — same wrong password doesn't stack penalty.
- Up to 10 checksums tracked per identity (2 inline, 8 pointer-based).
- Reconnect timeout: 5s minimum between reconnects.
- Query timeout: 5000 ms.

### Master FD

Backend processes write to FD 3 (`MASTER_ANVIL_FD`) directly (no socket needed once inherited).

---

## 13. yarilo-imap-hibernate — IMAP Idle Connection Parking

**Version**: `imap-master 1 0`
**Transport**: UNIX socket `{base_dir}/imap-master`
**Purpose**: Park IDLE IMAP connections to a separate process to free worker slots.

### Hibernation (Worker → Hibernate)

Worker passes FD + TAB-separated metadata:
```
<FD>\t<username>\thibernation_started=<sec>.<usec>\t[session=<id>]\t[tag=<imap_tag>]
    \t[lip=<ip>]\t[lport=<port>]\t[rip=<ip>]\t[rport=<port>]
    \t[state=<base64_imap_state>]\t[client_input=<base64_buffered>]
    \t[idle-continue]\t[bad-done]\n
```

Reply: `+\n` (success) or `-<error>\n`.

### Wakeup (Hibernate → Worker)

- Keepalive to client: `* OK Still here\r\n` at `imap_idle_notify_interval`.
- Move-back timeout: 10s (input pending) or 300s (change notification).
- Retry interval: 100 ms.

### IMAP Input States during hibernate

| State | Meaning |
|:---|:---|
| UNKNOWN | Incomplete data |
| BAD | Invalid IDLE sequence |
| DONE_LF | `DONE\n` |
| DONE_CRLF | `DONE\r\n` |
| DONEIDLE | `DONE\r\n<tag> IDLE\r\n` (keepalive continuation) |

---

## 14. yarilo-config — Config Service Protocol

**Version**: `config 2 0`
**Transport**: UNIX socket
**Purpose**: Centralized config export to all worker processes.

### Commands

```
REQ [service=<name>] [module=<name>] [exclude=<setting>] [lip=<ip>] [rip=<ip>]\n
```
Reply: `key=value\n` pairs then metadata flags, blank line to end.

```
FILTERS\n
```
Reply: `FILTER\t[service=<name>]\t[local-net=<ip>/<bits>]\t[remote-net=<ip>/<bits>]\n` ... blank line.

Setting value encoding: `\n` in value → `\x04` (SETTING_STREAM_LF_CHAR).

---

## 15. yarilo-log + yarilo-ipc — Log & IPC Services

### Log Service

Binary handshake struct sent immediately on connect:
```c
struct log_service_handshake {
    uint32_t log_magic;    // 0x02ff03fe
    uint32_t prefix_len;
    // unsigned char prefix[prefix_len];  // service name or "MASTER"
};
```

Log levels: DEBUG, INFO, WARNING, ERROR, FATAL, PANIC.
Fatal queue timeout: 500 ms. Max per-connection: 100 ms.

### IPC Service

**Version**: `ipc-proxy 1 0`
**Purpose**: Broadcast doveadm commands to all running processes.

```
Server → Client: HANDSHAKE\t<group_name>\t<pid>\n
Client → Server: <tag>\t<cmd>\n
Server → Client: <tag>\t:<data>\n   (info line)
                 <tag>\t+<data>\n   (success)
                 <tag>\t-<error>\n  (error)
```

Tag counter: auto-incrementing, never 0, wraps.

---

## 16. Master Process — Service Lifecycle

### FD Assignments (per service process)

| FD | Name | Purpose |
|:---|:---|:---|
| 3 | MASTER_ANVIL_FD | Write to anvil |
| 4 | MASTER_LOGIN_NOTIFY_FD | Login full notification (login services) |
| 5 | MASTER_STATUS_FD | Status reporting |
| 6 | MASTER_DEAD_FD | Detect master death (close-on-exit) |
| 7+ | MASTER_LISTEN_FD_FIRST | Listener sockets |

### Status Struct (written to FD 5)

```c
struct master_status {
    pid_t pid;
    unsigned int uid;              // GENERATION env var — validation
    unsigned int available_count;  // Connections this process will still accept
};
```

First status must arrive within 30s. Update on every `available_count` change.

### Key Environment Variables

| Variable | Purpose |
|:---|:---|
| `GENERATION` | UID for status message validation |
| `SERVICE_NAME` | imap, pop3, auth, etc. |
| `CLIENT_LIMIT` | Max concurrent clients per process |
| `PROCESS_LIMIT` | Max processes of this service type |
| `SERVICE_COUNT` | Connections before process self-exits |
| `IDLE_KILL` | Idle timeout seconds |
| `DOVECOT_VERSION` | Master version |
| `STATS_WRITER_SOCKET_PATH` | Stats socket path |

### Timeouts

```
MASTER_LOGIN_TIMEOUT_SECS          = 180
MASTER_AUTH_SERVER_TIMEOUT_SECS    = 150
MASTER_AUTH_LOOKUP_TIMEOUT_SECS    = 155
SERVICE_FIRST_STATUS_TIMEOUT_SECS  = 30
```

---

## 17. dsync — Backend Replication Protocol

**Version**: `dsync 3 5`
**Transport**: SSH pipe, TCP, or UNIX socket
**Format**: TAB-delimited, dot-stuffed binary streams for mail bodies.

### Minor Version Feature Bits

| Minor | Feature |
|:---|:---|
| 1 | Mailbox attributes |
| 2 | Save GUID |
| 3 | FINISH message |
| 4 | Header hash v2 |
| 5 | Header hash v3 |

### Message Type Characters

| Char | Type | Key Fields |
|:---|:---|:---|
| H | handshake | hostname, sync_type, backup_send/recv, lock_timeout |
| S | mailbox_state | mailbox_guid, last_uidvalidity, last_common_uid, last_common_modseq |
| N | mailbox_tree_node | name, existence, mailbox_guid, uid_validity |
| D | mailbox_delete | hierarchy_sep, mailboxes, dirs, unsubscribes |
| B | mailbox | mailbox_guid, uid_validity, uid_next, messages_count, highest_modseq |
| A | mailbox_attribute | type, key, value, deleted, last_change (v1+) |
| C | mail_change | type, uid, guid, hdr_hash, modseq, add_flags, remove_flags, keyword_changes |
| R | mail_request | guid or uid |
| M | mail | guid, uid, received_date, saved_date, stream (dot-stuffed body) |
| F | finish | error, mail_error, require_full_resync (v3+) |
| c | mailbox_cache_field | name, decision, last_used |
| . | end_of_list | (marker) |

### Handshake Exchange

1. Both sides: `VERSION\tdsync\t3\t5\n`
2. Both sides: serializer headers for each message type (`<char>\t<field1>\t<field2>\t...\n`)
3. Exchange ends with: `.\n`

### Mail Body Stream Format

```
M\t<guid>\t<uid>\t<received_date>\t<saved_date>\t<stream>\n
[raw message bytes, dot-stuffed]
\r\n.\r\n
```

Dot-stuffing: `.` after newline becomes `..`. Output buffer throttle: 128 KB.

### State File Format

Binary, base64-encoded, CRC32-validated:
```
4-byte header (major, minor, reserved×2)
Per mailbox: GUID(16) + uid_validity(4LE) + uid(4LE) + modseq(8LE) + pvt_modseq(8LE) + msg_count(4LE)
CRC32(4)
```

State file path: `{home}/.dovecot-sync` (configurable).

### GUID Hashing (no GUID backends)

MD5 of Date + Message-ID headers. Empty hash signature: `68b329da9893e34099c7d8ad5cb9c940` (MD5 of `\n`).

### Conflict Resolution

| Situation | Resolution |
|:---|:---|
| UIDVALIDITY mismatch | Keep side with higher message count |
| Same UID, different GUID | Replace with remote; set require_full_resync |
| Remote GUID in local expunges | Re-save from remote; set changes_during_sync |

### Key Constants

```
DSYNC_MAILBOX_DEFAULT_LOCK_TIMEOUT_SECS = 30
DSYNC_LOCK_FILENAME          = .dovecot-sync.lock
DSYNC_MAILBOX_LOCK_FILENAME  = .dovecot-box-sync.lock
```

---

## 18. Replication Daemon Protocol

**Transport**: UNIX socket `{base_dir}/replication-notify` + FIFO `{base_dir}/replication-notify-fifo`

### Mail Process → Replication

```
<username>\t<priority>\n
```

Priorities: `high` (new messages), `low` (flag changes/expunges), `sync` (wait for ack).

For sync priority: send on socket, wait for `+\n` (success) or `-<error>\n`. Timeout: 10s.

### Aggregator → Replicator Handshake

```
VERSION\treplicator-notify\t1\t0\n
```

### Queue Constants

```
REPLICATION_NOTIFY_DELAY_MSECS  = 500
REPLICATION_SYNC_TIMEOUT_SECS   = 10
REPLICATOR_RECONNECT_MSECS      = 5000
REPLICATOR_MEMBUF_MAX_SIZE      = 1 MB
```

Priority order (highest first): SYNC(3) > HIGH(2) > LOW(1) > NONE(0).

---

## 19. Indexer Service Protocol

**Version**: `1 0` (handshake: `1\t0\tindexer\tindexer\n`)
**Transport**: UNIX socket `{base_dir}/indexer`

### Client Commands

```
PREPEND\t<tag>\t<user>\t<mailbox>\t[<max_recent_msgs>]\t[<session_id>]\n  (high priority)
APPEND\t<tag>\t<user>\t<mailbox>\t[<max_recent_msgs>]\t[<session_id>]\n   (low priority)
OPTIMIZE\t<tag>\t<user>\t<mailbox>\n
```

- `tag = 0`: no status updates; `tag > 0`: client wants progress.
- Status reply during indexing: `<tag>\t<percentage>\n` (0–99 = progress, 100 = done, -1 = failed).
- ACK on receipt: `<tag>\tOK\n`.

### Worker Protocol

```c
// Worker → Master handshake reply: <process_limit>\n
// Master → Worker: <username>\t<mailbox>\t[<session_id>]\t<max_recent_msgs>\t<flags>\n
//   flags: 'i' = index, 'o' = optimize
// Worker progress: <percentage>\n
```

### Constants

```
INDEXER_NOTIFY_INTERVAL_SECS = 10
INDEXER_WAIT_MSECS           = 250
MAX_INBUF_SIZE               = 65536 (64 KB)
```

---

## 20. FTS Backend API

### Backend Interface (fts_backend_vfuncs)

```
alloc / init / deinit
get_last_uid(box)                              → last indexed UID
update_init / update_deinit
update_set_mailbox(ctx, box)
update_expunge(ctx, uid)
update_set_build_key(ctx, key) → bool
update_unset_build_key(ctx)
update_build_more(ctx, data, size)
refresh / rescan / optimize
can_lookup(args) → bool
lookup(box, args, flags, result)
lookup_multi(boxes[], args, flags, result)
lookup_done()
```

### Build Key Types

```
FTS_BACKEND_BUILD_KEY_HDR              // message header
FTS_BACKEND_BUILD_KEY_MIME_HDR         // MIME part header
FTS_BACKEND_BUILD_KEY_BODY_PART        // text MIME body
FTS_BACKEND_BUILD_KEY_BODY_PART_BINARY // binary body
```

### Backend Capability Flags

```
FTS_BACKEND_FLAG_BINARY_MIME_PARTS  = 0x01
FTS_BACKEND_FLAG_NORMALIZE_INPUT    = 0x02
FTS_BACKEND_FLAG_BUILD_FULL_WORDS   = 0x04
FTS_BACKEND_FLAG_FUZZY_SEARCH       = 0x08
FTS_BACKEND_FLAG_TOKENIZED_INPUT    = 0x10
```

### Tokenization Pipeline

```
Input text → Tokenizer (generic | email_address)
           → Filters chain: stopwords → stemmer_snowball → normalizer_icu → lowercase → english_possessive
           → update_build_more()
```

ICU normalizer default rule: `Any-Lower; NFKD; [: Nonspacing Mark :] Remove; NFC`
Max token size: 1024 bytes.

### FTS Index Header (in mail index extensions)

```c
struct fts_index_header {
    uint32_t last_indexed_uid;     // Incremental indexing boundary
    uint32_t settings_checksum;    // Rebuild on change
    uint32_t unused;
};
```

### Squat (Built-in FTS) File Format

File prefix: `dovecot.index.search` + `.uidlist`.

```c
struct squat_file_header {
    uint8_t  version;           // 2
    uint8_t  unused[3];
    uint32_t indexid;
    uint32_t uidvalidity;
    uint32_t used_file_size;
    uint32_t deleted_space;
    uint32_t node_count;
    uint32_t root_offset;
    uint8_t  partial_len;
    uint8_t  full_len;
    uint8_t  normalize_map[256];
};
```

Variable-length integer packing: high bit set = continues, low 7 bits = value.

### Solr HTTP API

```
GET  /solr/update/select?wt=xml&fl=uid,score&rows=100000&q=<query>
POST /solr/update  Content-Type: text/xml

Index payload:
<add><doc>
  <field name="id"><uid>/<box_guid>[/<user>]</field>
  <field name="uid"><uid></field>
  <field name="box"><box_guid></field>
  <field name="user"><user></field>
  <field name="hdr_Subject">...</field>
  <field name="body">...</field>
</doc></add>
<commit/>

Delete: <delete><query>uid:<uid> AND box:<box_guid></query></delete>
```

HTTP settings: connect 5s, request 60s, max 3 attempts, 1 redirect, 1 parallel connection.
Solr escape chars: `+-&|!(){}[]^"~*?:\\/ `.

---

## 21. LMTP Delivery Protocol

### State Machine

```
SMTP_SERVER_STATE_GREETING → XCLIENT → HELO → STARTTLS → AUTH → READY
  → MAIL_FROM → RCPT_TO → DATA
```

### Key Differences vs SMTP

- Per-recipient reply for DATA (`SMTP_SERVER_TRANSACTION_FLAG_REPLY_PER_RCPT`)
- `auth_optional = true`
- `rcpt_domain_optional = true` (local-part only allowed)
- `mail_path_allow_broken = true` (empty sender `<>` allowed)

### Recipient Types

```
LMTP_RECIPIENT_TYPE_LOCAL  — local mailbox delivery
LMTP_RECIPIENT_TYPE_PROXY  — proxied to remote LMTP
```

### Quota Status Codes

```
452 4.2.2  — quota failure (if quota_full_tempfail = true)
552 5.2.2  — quota failure (permanent)
451 4.3.0  — too many concurrent deliveries (anvil limit)
```

### Delivery Pipeline

1. `RCPT` → quota check → anvil concurrency check
2. `DATA` → temp file via `iostream_temp_create_named()` (max in-memory: 128 KB)
3. Header injection (Received:) if `lmtp_add_received_header = true`
4. Concatenate injected headers + data stream
5. Sieve execution (via `mail_deliver_hooks`)
6. Save to mailbox

### XRCPTFORWARD Custom Extension

```
RCPT TO:<addr> XRCPTFORWARD=<base64-encoded>
```
Only accepted from trusted IPs. Contains passdb forward fields.

### Session ID Pattern

- First recipient: `trans->id`
- Subsequent: `{trans->id}:R{counter}`
- Quota check suffix: `":quota"`

---

## 22. SMTP + lib-smtp Details

### Wire Format Limits

```
Base line length:          510 bytes (+ CRLF = 512)
Command parameters:        4 KB  (SMTP_COMMAND_DEFAULT_MAX_PARAMETERS_SIZE)
AUTH response:             8 KB
Message data:              40 MB (SMTP_COMMAND_DEFAULT_MAX_DATA_SIZE)
XCLIENT line:              512 bytes (split into multiple if exceeded)
In-memory DATA buffer:     128 KB
```

### Capability Flags

```
SMTP_CAPABILITY_AUTH                = BIT(0)
SMTP_CAPABILITY_STARTTLS            = BIT(1)
SMTP_CAPABILITY_PIPELINING          = BIT(2)
SMTP_CAPABILITY_SIZE                = BIT(3)
SMTP_CAPABILITY_ENHANCEDSTATUSCODES = BIT(4)
SMTP_CAPABILITY_8BITMIME            = BIT(5)
SMTP_CAPABILITY_CHUNKING            = BIT(6)
SMTP_CAPABILITY_BINARYMIME          = BIT(7)
SMTP_CAPABILITY_BURL                = BIT(8)
SMTP_CAPABILITY_DSN                 = BIT(9)
SMTP_CAPABILITY_XCLIENT             = BIT(12)
```

### MAIL Parameters

```
AUTH=<address>             RFC 4954
BODY=7BIT|8BITMIME|BINARYMIME  RFC 6152
ENVID=<id>                 RFC 3461 (DSN)
RET=HDRS|FULL              RFC 3461
SIZE=<octets>              RFC 1870
```

### RCPT Parameters

```
ORCPT=rfc822;<addr>        RFC 3461
NOTIFY=SUCCESS|FAILURE|DELAY|NEVER  RFC 3461
```

### BDAT / CHUNKING (RFC 3030)

```
BDAT <size>\r\n<chunk>
BDAT <size> LAST\r\n<chunk>
```
BINARYMIME requires BDAT (cannot use plain DATA).

### XCLIENT Wire Format (Submission Proxy)

```
XCLIENT PROTO=ESMTP ADDR=<ip> PORT=<port> HELO=<domain> LOGIN=<user>
        SESSION=<id> TTL=<n> FORWARD=<base64>
```
Values xtext-encoded (unreserved: 0-9 A-Z a-z - _; others as `+XX`).
If line >512 bytes, split into multiple XCLIENT commands.

### Submission Server (Port 587)

Supported capabilities:
```
AUTH | PIPELINING | SIZE | ENHANCEDSTATUSCODES | 8BITMIME |
CHUNKING | BINARYMIME | BURL | DSN | VRFY
```

Constants:
```
SUBMISSION_MAX_ADDITIONAL_MAIL_SIZE     = 1024 bytes
SUBMISSION_MAIL_DATA_MAX_INMEMORY_SIZE  = 128 KB
SUBMISSION_MAX_WAIT_QUIT_REPLY_MSECS    = 2000 ms
```

Submission proxy state machine:
```
BANNER → EHLO → [STARTTLS → TLS_EHLO] → [XCLIENT → XCLIENT_EHLO] → AUTHENTICATE
```

Client Timeouts:
```
SMTP_DEFAULT_CONNECT_TIMEOUT_MSECS  = 30 000 (30s)
SMTP_DEFAULT_COMMAND_TIMEOUT_MSECS  = 300 000 (5 min)
```

---

## 23. IMAP Server Internals

### Command Queue

```
CLIENT_COMMAND_QUEUE_MAX_SIZE = 4
CLIENT_MAX_BAD_COMMANDS       = 20
CLIENT_IDLE_TIMEOUT_MSECS     = 1 800 000 (30 min)
CLIENT_OUTPUT_TIMEOUT_MSECS   = 300 000 (5 min)
CLIENT_OUTPUT_OPTIMAL_SIZE    = 2048 bytes
```

### Command States

```
WAIT_INPUT / WAIT_OUTPUT / WAIT_EXTERNAL / WAIT_UNAMBIGUITY / WAIT_SYNC / DONE
```

### IDLE (RFC 2177)

- Entry: send `+ idling`, set `cmd_idle_continue` as handler.
- Keepalive: `* OK Still here\r\n` at `imap_idle_notify_interval`.
- Hibernation: after `imap_hibernation_timeout` → park to imap-hibernate process.
- Exit: client sends `DONE\r\n`.

### NOTIFY Extension Events (RFC 5465)

```
MESSAGE_NEW        = 0x01
MESSAGE_EXPUNGE    = 0x02
FLAG_CHANGE        = 0x04
ANNOTATION_CHANGE  = 0x08  (unsupported)
MAILBOX_NAME       = 0x10
SUBSCRIPTION_CHANGE = 0x20
MAILBOX_METADATA_CHANGE = 0x40  (unsupported)
SERVER_METADATA_CHANGE  = 0x80  (unsupported)
```

Watch types: SUBSCRIBED, SUBTREE, MAILBOX.
Constraint: MessageNew and MessageExpunge must both be specified together.

### CONDSTORE / QRESYNC

- MODSEQ: 64-bit counter, persisted per mailbox.
- `CHANGEDSINCE <modseq>` in FETCH: only messages with modseq > value.
- QRESYNC SELECT parameters: UIDVALIDITY, known-modseq, known-seq-set, known-uid-set.
- `VANISHED [EARLIER]` response: untagged, lists expunged UIDs.

### FETCH Macros

```
ALL  = FLAGS INTERNALDATE RFC822.SIZE ENVELOPE
FAST = FLAGS INTERNALDATE RFC822.SIZE
FULL = FLAGS INTERNALDATE RFC822.SIZE ENVELOPE BODY
```

### BINARY Extension (RFC 3516)

Literal8 format: `~{size}\r\n<binary data with NULs>`.
`BINARY.SIZE[section]` — returns size without transmitting body.

---

## 24. POP3 Server Internals

### Session Lock

File: `dovecot-pop3-session.lock` in mailbox directory.
Stale timeout: `POP3_SESSION_DOTLOCK_STALE_TIMEOUT_SECS = 300` (5 min).
One exclusive session per mailbox — concurrent POP3 rejected.

### Timeouts

```
CLIENT_IDLE_TIMEOUT_MSECS    = 600 000 (10 min)
CLIENT_COMMIT_TIMEOUT_MSECS  = 10 000 (10s — auto-commit transaction)
CLIENT_MAX_BAD_COMMANDS      = 20
POP3_OUTBUF_THROTTLE_SIZE    = 4096 bytes (pause input when output buffer full)
```

### UIDL Format

Configurable `uidl_keymask` bitmask:
```
UIDL_UIDVALIDITY = 0x01  — mailbox UID validity
UIDL_UID         = 0x02  — message UID
UIDL_MD5         = 0x04  — MD5 of message size
UIDL_FILE_NAME   = 0x08  — filename
UIDL_GUID        = 0x10  — message GUID
```

### Message Numbering

- POP3 MSN: 1-based (protocol-visible).
- Internal msgnum: 0-based.
- Storage seq: 1-based.
- Deletion bitmask: `deleted[msgnum/8] & (1 << (msgnum%8))`.

### CAPA Response

```
CAPA / TOP / UIDL / RESP-CODES / PIPELINING / AUTH-RESP-CODE / SASL mechanisms
```

---

## 25. lib-imap Parser Details

### Argument Types

```
IMAP_ARG_NIL / IMAP_ARG_ATOM / IMAP_ARG_STRING / IMAP_ARG_LIST /
IMAP_ARG_LITERAL / IMAP_ARG_LITERAL_SIZE / IMAP_ARG_LITERAL_SIZE_NONSYNC / IMAP_ARG_EOL
```

### Literal Types

| Format | Type | Max size |
|:---|:---|:---|
| `{size}\r\n` | Synchronizing (server sends `+`) | unlimited |
| `{size+}\r\n` | Non-synchronizing (LITERAL+/LITERAL-) | 4096 bytes |
| `~{size}\r\n` | LITERAL8 — binary with NULs (BINARY ext) | unlimited |

### Parser Flags

```
IMAP_PARSE_FLAG_LITERAL_SIZE        = 0x01  return size only
IMAP_PARSE_FLAG_NO_UNESCAPE         = 0x02
IMAP_PARSE_FLAG_LITERAL_TYPE        = 0x04
IMAP_PARSE_FLAG_ATOM_ALLCHARS       = 0x08  don't validate atom chars
IMAP_PARSE_FLAG_MULTILINE_STR       = 0x10
IMAP_PARSE_FLAG_INSIDE_LIST         = 0x20
IMAP_PARSE_FLAG_LITERAL8            = 0x40
IMAP_PARSE_FLAG_SERVER_TEXT         = 0x80
IMAP_PARSE_FLAG_STOP_AT_LIST        = 0x100
```

### NIL vs Empty String

- NIL: `imap_arg_type == IMAP_ARG_NIL` → `imap_arg_get_nstring()` returns str=NULL.
- Empty string: `IMAP_ARG_STRING` with len=0 — different semantic from NIL.

---

## 26. LDA Delivery Pipeline

### Duplicate Detection

DB: `mail_duplicate_db`.
Fields tracked: Message-ID, From, Subject, Date, storage GUID.
Session dedup: `inbox_guids[]` — prevents duplicate saves within one LMTP session.

### Reject vs Bounce

```
MAIL_DELIVER_ERROR_REJECTED   — refuse, trigger DSN bounce to sender
MAIL_DELIVER_ERROR_NOQUOTA    — quota exceeded
MAIL_DELIVER_ERROR_TEMPORARY  — 4xx, retry
MAIL_DELIVER_ERROR_INTERNAL   — 5xx, no retry
```

### Sieve Hook

Registered via `mail_deliver_hooks_init()`. Runs before final mailbox save.
Can: redirect, fileinto, reject, vacation, add flags.

---

## 27. OAuth2 / SASL Internals

### XOAUTH2 Payload (Google proprietary)

Base64-encoded JSON:
```json
{"user":"user@domain","auth":"Bearer <token>"}
```
Flow: `AUTHENTICATE XOAUTH2` → `+` → client sends base64(JSON).
On failure: server sends base64(JSON error) → client sends empty response `\r\n`.

### OAUTHBEARER Payload (RFC 6750)

```
n,a=<authid>,\x01host=<host>\x01port=<port>\x01auth=Bearer <token>\x01\x01
```

### OAuth2 Introspection Modes

```
INTROSPECTION_MODE_GET_AUTH  — GET with Authorization header
INTROSPECTION_MODE_GET       — GET with token parameter
INTROSPECTION_MODE_POST      — POST with token=<token>
INTROSPECTION_MODE_LOCAL     — local JWT validation (no network)
```

JWT claims: `sub`, `aud`, `exp`, `iat`, `iss`. Key cache: `oauth2_validation_key_cache`.

### SASL PLAIN Payload

```
[authzid]\0authid\0password
Empty authzid: \0user@domain\0password
```

### SCRAM-SHA-256 Key Derivation

```
SaltedPassword = PBKDF2(password, salt, iterations, SHA256)
ClientKey      = HMAC(SaltedPassword, "Client Key")
ServerKey      = HMAC(SaltedPassword, "Server Key")
ClientProof    = XOR(ClientKey, HMAC(ClientKey, auth_message))
```

---

## 28. IMAP URLAUTH Service

**Transport**: UNIX socket `{base_dir}/imap-urlauth`

### URL Format

```
imap://user@host/INBOX/;uid=1234/;section=TEXT/;urlauth=INTERNAL:<token>
```

### Commands

```
GENURLAUTH <rump-url> INTERNAL [...]
  → * GENURLAUTH <full-url> [...]

URLFETCH <url> [...]
  → * URLFETCH <url> NIL | <literal>

RESETKEY [mailbox | ()]
```

Token: HMAC-based, per-mailbox secret key. `RESETKEY` rotates the key, invalidating existing tokens.

---

## 29. lib-fs Abstraction (for obox / S3)

### Core Interface Methods

```
alloc / init / deinit / free
get_properties() → enum fs_properties
file_init / file_deinit / file_close / get_path
set_async_callback / wait_async
set_metadata / get_metadata
prefetch() → bool (true = in memory)
read() → ssize_t
read_stream() → istream
write() → int
write_stream() → ostream
write_stream_finish() → 1 (ok) | 0 (async pending) | -1 (error)
lock / unlock
exists() → 1|0|-1
stat() → struct stat
copy / rename / delete_file
iter_alloc / iter_init / iter_next / iter_deinit
switch_ioloop / get_nlinks
```

### fs_properties Flags

```
FS_PROPERTY_METADATA     = 0x01
FS_PROPERTY_LOCKS        = 0x02
FS_PROPERTY_FASTCOPY     = 0x04
FS_PROPERTY_RENAME       = 0x08
FS_PROPERTY_STAT         = 0x10
FS_PROPERTY_ITER         = 0x20
FS_PROPERTY_RELIABLEITER = 0x40
FS_PROPERTY_DIRECTORIES  = 0x80
FS_PROPERTY_WRITE_HASH_MD5    = 0x100
FS_PROPERTY_WRITE_HASH_SHA256 = 0x200
FS_PROPERTY_ASYNC        = 0x800
FS_PROPERTY_OBJECTIDS    = 0x1000
```

### Open Modes

```
FS_OPEN_MODE_READONLY             — fail if not exists
FS_OPEN_MODE_CREATE               — fail if exists
FS_OPEN_MODE_CREATE_UNIQUE_128    — auto-generated 128-bit hex name
FS_OPEN_MODE_REPLACE              — create or overwrite
FS_OPEN_MODE_APPEND
```

### Metadata Constants

```
FS_METADATA_INTERNAL_PREFIX = ":/X-Dovecot-fs-api-"
FS_METADATA_OBJECTID        = ":/X-Dovecot-fs-api-ObjectID"
FS_METADATA_WRITE_FNAME     = ":/X-Dovecot-fs-api-WriteFilename"
FS_METADATA_ORIG_PATH       = ":/X-Dovecot-fs-api-OrigPath"
```

Internal prefix keys are NOT sent to the storage backend.

### Atomic Write Pattern (S3 equivalent)

1. Open with `FS_OPEN_MODE_REPLACE`
2. Get ostream via `fs_write_stream()`
3. Write data
4. Optionally set `FS_METADATA_WRITE_FNAME` (final key) before finish
5. Call `fs_write_stream_finish()` → atomic rename to final key
6. For async: retry `fs_write_stream_finish_async()` until 1

### Iteration Flags

```
FS_ITER_FLAG_DIRS     = 0x01  dirs only
FS_ITER_FLAG_ASYNC    = 0x02
FS_ITER_FLAG_OBJECTIDS = 0x04  return <objectid>/<name>
FS_ITER_FLAG_NOCACHE  = 0x08
```

---

## 30. lib-http Client

### Key Settings

```go
max_idle_time_msecs           // idle connection reuse timeout
max_parallel_connections      // per host:port (default 1)
max_pipelined_requests        // per connection (default 1)
max_redirects                 // 0 = refuse redirects
max_attempts                  // retry count
connect_timeout_msecs         // TCP connect
soft_connect_timeout_msecs    // try next IP after this
request_timeout_msecs         // per attempt
request_absolute_timeout_msecs // total request time
connect_backoff_time_msecs
connect_backoff_max_time_msecs
```

### Request States

```
NEW → QUEUED → PAYLOAD_OUT → WAITING → GOT_RESPONSE → PAYLOAD_IN → FINISHED
                                                                   → ABORTED
```

### Response

```c
struct http_response {
    unsigned int status;       // HTTP status code; 9000+ = client-side error
    const char *reason;
    const char *location;      // from Location header (for redirects)
    time_t date, retry_after;
    const struct http_header *header;
    struct istream *payload;   // response body — must be read/discarded
    bool connection_close;
};
// http_response_is_success: status/100 == 2
```

HTTP internal error constant: `HTTP_RESPONSE_STATUS_INTERNAL = 9000`.

---

## 31. lib-ssl / TLS + SNI

### SNI Pattern (per-domain certificates)

```
1. Server creates ssl_iostream with SNI callback registered.
2. During handshake: sni_callback(name, error_r, ctx) is called.
3. Callback loads domain-specific ssl_iostream_context.
4. ssl_iostream_change_context(ssl_io, new_ctx) switches cert.
5. Handshake continues with the new context.
```

### Settings

```c
struct ssl_iostream_settings {
    const char *min_protocol;   // "TLSv1.2" or "TLSv1.3"
    const char *cipher_list;    // OpenSSL cipher string
    const char *ciphersuites;   // TLSv1.3 only
    bool prefer_server_ciphers;
    bool verify_remote_cert;
    bool allow_invalid_cert;    // stream-only
    bool tickets;               // session tickets
    struct ssl_iostream_cert cert;      // cert + key + key_password
    struct ssl_iostream_cert alt_cert;  // alternative algorithm cert
};
```

### TLS Information

```
ssl_iostream_get_protocol_name()  → "TLSv1.2", "TLSv1.3"
ssl_iostream_get_cipher()         → cipher name + bits
ssl_iostream_get_pfs()            → "DH", "ECDH", etc.
ssl_iostream_get_peer_name()      → CN from peer cert
ssl_iostream_get_security_string()
```

Context caching: `ssl_iostream_server_context_cache_get()` — reuse across connections.

---

## 32. Dict Protocol (Extended)

**Version**: `3 2` (DICT_CLIENT_PROTOCOL_MAJOR_VERSION=3, MINOR=2)
**Max line**: 64 KB
**Multi-value support**: minor ≥ 2

### Full Command Set

```
H<major> <minor> <value_type> <user> <dict_name>\n  HELLO
L<key>\n                                             LOOKUP
I<flags> <max_rows> <path> <user>\n                  ITERATE
B<txn_id> <user>\n                                   BEGIN transaction
C<txn_id>\n                                          COMMIT
D<txn_id>\n                                          COMMIT_ASYNC
R<txn_id>\n                                          ROLLBACK
S<txn_id>\t<key>\t<value>\n                          SET
U<txn_id>\t<key>\n                                   UNSET
A<txn_id>\t<key>\t<diff>\n                           ATOMIC_INC (signed int64)
T<txn_id>\t<secs>\t<nsecs>\n                         TIMESTAMP (Cassandra)
V<txn_id>\t<0|1>\n                                   HIDE_LOG_VALUES
```

### Reply Set

```
O<value>\n          OK — found/committed
M\t<v1>\t<v2>...\n  MULTI_OK — multiple values (v2.2+)
N\n                 NOTFOUND
F<error>\n          FAIL
W<error>\n          WRITE_UNCERTAIN (write may have succeeded)
A\n                 ASYNC_COMMIT succeeded
*<async_id>\n       ASYNC_ID prefix before async reply
+<async_id>\t<reply>\n  ASYNC_REPLY (unordered)
```

ITERATE streaming: `O<key>\t<value>\n` per row, `\n` on completion.

### Key Namespacing

```
priv/<key>    — user-specific (user scope)
shared/<key>  — global (shared)
```

Key escaping: `/` → `\-`, `\` → `\\` (for usernames in paths).

### Redis Backend Wire Protocol

```
AUTH   *2\r\n$4\r\nAUTH\r\n$<n>\r\n<password>\r\n
SELECT *2\r\n$6\r\nSELECT\r\n$<n>\r\n<db>\r\n
GET    *2\r\n$3\r\nGET\r\n$<n>\r\n<key>\r\n
SET    *3\r\n$3\r\nSET\r\n$<n>\r\n<key>\r\n$<n>\r\n<value>\r\n
DEL    *2\r\n$3\r\nDEL\r\n$<n>\r\n<key>\r\n
EXPIRE *3\r\n$6\r\nEXPIRE\r\n$<n>\r\n<key>\r\n$<n>\r\n<secs>\r\n
INCRBY *3\r\n$6\r\nINCRBY\r\n$<n>\r\n<key>\r\n$<n>\r\n<diff>\r\n
MULTI  *1\r\n$5\r\nMULTI\r\n
EXEC   *1\r\n$4\r\nEXEC\r\n
```

Redis defaults: port 6379, lookup timeout 30s.
Transaction: MULTI → SET/DEL/INCRBY (each returns QUEUED) → EXEC → `*<n>` replies.

---

## 33. Plugin Internals

### mail-crypt

Magic bytes (file header): `CRYPTED\x03\x07` (9 bytes).
Algorithm: `aes-256-gcm-sha256` (AEAD). IOSTREAM_CRYPT_VERSION = 2.

Key storage in mailbox attributes:
```
shared/<mailbox_guid>/.../crypt/active          → active public key digest
shared/<mailbox_guid>/.../crypt/pubkeys/<digest> → public key
private/<mailbox_guid>/.../crypt/privkeys/<digest> → private key (encrypted)
```

User-level keys: same paths but with INBOX GUID.

Key derivation: EC key pair on curve (e.g. P-256). Key cipher: `ecdh-aes-256-ctr`. Key ID: SHA256.
Encryption outer, compression inner (ordering matters in stream wrapping).
Cache: per-user, timeout 60s (`MAIL_CRYPT_MAIL_CACHE_EXPIRE_MSECS`).

### zlib (per-message compression)

Magic bytes: detected by `compression_detect_handler()` — algorithm-specific headers.
Config: `zlib_save = gz|bz2|lz4`, `zlib_save_level = <n>`.
Decompression wraps innermost. Compression wraps outermost.
Cache: per-user seekable stream, timeout 60s (`ZLIB_MAIL_CACHE_EXPIRE_MSECS`).

### lazy-expunge

Config: `lazy_expunge = <namespace_prefix>`.
On expunge: move message to expunge folder with `MAILBOX_FLAG_SAVEONLY | MAILBOX_FLAG_NO_INDEX_FILES | MAILBOX_FLAG_IGNORE_ACLS`.
With `lazy_expunge_only_last_instance`: dedup by GUID — only move if last copy.

### push-notification

Event types: `save`, `append`, `expunge`, `flagchange`, `keywordchange`, mailbox `create/delete/rename/subscribe/unsubscribe`.

Driver vfuncs: `init / begin_txn / process_mbox / process_msg / end_txn / deinit / cleanup`.

Built-in drivers: `ox` (HTTP POST JSON), `xaps` (Apple XAPS), `dlog` (debug log), `lua`.

### virtual mailboxes

Config file: `dovecot-virtual` in the virtual mailbox directory.
Format: mailbox patterns (one per line, glob) + search query (IMAP search syntax).
Prefix chars: `+` (clear recent), `-` (exclude), `!` (APPEND target), `/metadata:value` (metadata match).
Search args CRC32 stored in index for cache invalidation.

### quota-clone

Dict paths: `priv/quota/storage` (bytes), `priv/quota/messages` (count).
Flush delay: `QUOTA_CLONE_FLUSH_DELAY_MSECS = 10 000` ms (async, pre-deinit flush).
Clones first quota root only.

### last-login

Dict path: `shared/last-login/<username>`.
Precision: `s` (seconds), `ms`, `us`, `ns`.
Format for `ms`: `{sec}{msec_3digits}`.

### pop3-migration

UIDL mapping: 3-phase: (1) cached UIDLs, (2) size-based, (3) SHA1 of filtered headers.
Skip-list headers: `Content-Length, Return-Path, Status, X-IMAP, X-IMAPbase, X-Keywords, X-Message-Flag, X-Status, X-UID, X-UIDL, X-Yahoo-Newman-Property`.
Cache field: `pop3-migration.hdr` (20 bytes = SHA1_RESULTLEN) in mail index.
