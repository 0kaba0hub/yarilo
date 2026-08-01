# IMAP configuration

Yarilo implements IMAP4rev2 (RFC 9051) with backward compatibility to IMAP4rev1 (RFC 3501).

Listeners: `services.imaps` (implicit TLS, port 993) and `services.imap` (STARTTLS, port 143).
See [SERVICES.md](SERVICES.md) for listener-level settings (port, ssl_mode, haproxy_protocol, etc.).

---

## `protocol.imap`

Protocol-level behaviour, shared across both IMAP listeners.

| Key | Default | Description |
|:---|:---|:---|
| `imap_idle_notify_interval` | `120` | Seconds between unsolicited EXISTS/RECENT responses during IDLE (RFC 2177 keepalive). `0` = disabled. |
| `imap_max_line_length` | `65536` | Max IMAP command line length in bytes (64 KB = the reference default). `0` = unlimited. |
| `imap_id_send` | `name *` | Space-separated key-value pairs sent in the ID response (RFC 2971). `*` = server-default values. Empty string = ID extension disabled. |
| `login_greeting` | `""` | Custom text appended to the server greeting replacing the default `Yarilo IMAP server ready`. Empty = default greeting. |
| `imap_logout_format` | `""` | Format string logged at session end. Empty = no stats line. Variables listed below. |

### `imap_logout_format` variables

| Variable | Description |
|:---|:---|
| `%{deleted}` | Number of messages marked `\Deleted` in the session. |
| `%{expunged}` | Number of messages expunged (removed from disk). |
| `%{fetch_hdr_count}` | Number of header fetch operations. |
| `%{fetch_hdr_bytes}` | Bytes transferred for header fetches. |
| `%{fetch_body_count}` | Number of body fetch operations. |
| `%{fetch_body_bytes}` | Bytes transferred for body fetches. |

Example:

```yaml
protocol:
  imap:
    imap_logout_format: "in=%{fetch_hdr_bytes}+%{fetch_body_bytes} expunged=%{expunged}"
```

---

## Supported IMAP extensions

| Extension | RFC | Notes |
|:---|:---|:---|
| IDLE | RFC 2177 | Server-push new-mail notifications. |
| MOVE | RFC 6851 | Atomic move (no COPY + STORE + EXPUNGE round-trip). |
| CONDSTORE | RFC 7162 | `MODSEQ` flag, conditional STORE. |
| QRESYNC | RFC 7162 | Fast mailbox resync after reconnect. |
| UIDPLUS | RFC 4315 | `APPENDUID` / `COPYUID` response codes. |
| UNSELECT | RFC 3691 | Close mailbox without expunge. |
| NAMESPACE | RFC 2342 | Shared / Other Users namespaces. |
| QUOTA | RFC 9208 | Per-user storage quota. |
| ACL | RFC 4314 | Per-mailbox access control lists. |
| BINARY | RFC 3516 | Binary content transfer. |
| SORT | RFC 5256 | Server-side message sorting. |
| THREAD | RFC 5256 | Threading by subject / references. |
| ESEARCH | RFC 4731 | Extended SEARCH with MIN/MAX/COUNT. |
| NOTIFY | RFC 5465 | Event-based notifications. `NOTIFY SET`/`NONE` parsed with all mailbox filters; the **selected** mailbox honours `SELECTED` / `SELECTED-DELAYED` (MessageNew / MessageExpunge / FlagChange), suppressing the unsolicited responses the client did not request (RFC 5465 §5). **Non-selected** mailbox filters (PERSONAL / INBOXES / SUBSCRIBED / SUBTREE / MAILBOXES) are watched via the `pkg/locks` event bus and their MessageNew / MessageExpunge / FlagChange activity is reported as untagged `* STATUS` (RFC 5465 §6), delivered during IDLE and before the next command's tagged response. The watched set is re-evaluated dynamically: mailboxes created, renamed or subscribed after `NOTIFY SET` join or leave the set live (via the per-user `mlist:` event key). Mailbox-level events — **MailboxName** (create / delete / rename, the last with `OLDNAME`) and **SubscriptionChange** (subscribe / unsubscribe) — are reported as untagged `* LIST` responses (RFC 5465 §5) when requested. `AnnotationChange` / metadata events are not yet reported. |
| URLAUTH | RFC 4467 | Authorised URL for CATENATE/BURL. |
| SPECIAL-USE | RFC 6154 | `\Sent`, `\Drafts`, `\Trash` folder flags. |
| ID | RFC 2971 | Server identity advertisement. |
| OBJECTID | RFC 8474 | Stable object identifiers: `MAILBOXID` (SELECT/EXAMINE response code + STATUS item, from the folder GUID), `EMAILID` (FETCH, from the message GUID), `THREADID` (FETCH, always `NIL` — no threading). IDs are 32 lowercase hex chars of the 128-bit GUID; they survive RENAME. |
| METADATA | RFC 5464 | Server and per-mailbox annotations (GETMETADATA / SETMETADATA). State lives in `cfg.Dicts["metadata"]` (`pkg/dict`). Keys: `priv/box/<folder_guid>/<entry>` and `shared/box/<folder_guid>/<entry>`; server-scope entries live under INBOX's GUID with a `vendor/yarilo/pvt/server/` prefix so they cannot collide with INBOX mailbox attributes. |
