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
```

### Server → Client Responses:
```
OK\t<id>\tuser=<username>\t[home=<dir>]\t[mail=<loc>]\t[uid=<n>]\t[gid=<n>]\t[proxy]\t[proxy_maybe]\t[host=<h>]\t[port=<p>]\t[destuser=<u>]\t[pass=<p>]\t[nologin]\n
FAIL\t<id>\t[temp_fail]\t[authz_fail]\t[user_disabled]\t[pass_expired]\t[reason=<str>]\n
CONT\t<id>\t<base64_challenge>\n
```

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
