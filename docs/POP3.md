# POP3 configuration

Yarilo implements POP3 (RFC 1939) with STLS (RFC 2595), UIDL, CAPA, and XCLIENT.

Listeners: `services.pop3s` (implicit TLS, port 995) and `services.pop3` (STARTTLS, port 110).
See [SERVICES.md](SERVICES.md) for listener-level settings (port, ssl_mode, haproxy_protocol, etc.).

---

## `protocol.pop3`

Protocol-level behaviour, shared across both POP3 listeners.

| Key | Default | Description |
|:---|:---|:---|
| `pop3_no_flag_updates` | `false` | `false` = set `\Seen` on RETR'd messages at QUIT (the reference default). `true` = no flag changes on retrieval. |
| `pop3_reuse_xuidl` | `false` | Use the `X-UIDL` message header as the UIDL value. Enables migration from Courier / qmail / cPanel without UIDL changes. |
| `pop3_uidl_format` | `%u.%v` | UIDL format string. See variables below. |
| `pop3_uidl_duplicates` | `rename` | `allow` = emit duplicate UIDLs as-is. `rename` = append `-N` suffix to guarantee uniqueness. |
| `pop3_enable_last` | `false` | Advertise and handle the `LAST` command (RFC 1460). |
| `pop3_delete_type` | `expunge` | `expunge` = remove message from disk at QUIT. `flag` = set `pop3_deleted_flag` (soft delete, keeps message in IMAP). |
| `pop3_deleted_flag` | `""` | IMAP flag to set when `pop3_delete_type: flag`. Example: `$POP3Deleted`. |
| `pop3_save_uidl` | `false` | Persist computed UIDL values to `$HOME/.<folder>/pop3.uidl`. Subsequent sessions load the saved values so UIDLs remain stable across index rebuilds or backend changes. |
| `pop3_lock_session` | `false` | Create a dotlock file at `$HOME/yarilo-pop3-session.lock` after login. Prevents simultaneous access from another yarilo-pop3 replica on the same PVC. Stale locks (older than 10 min) are stolen automatically. |

### `pop3_uidl_format` variables

| Variable | Description |
|:---|:---|
| `%u` | Message UID. |
| `%v` | Mailbox UIDValidity. |
| `%f` | Filename (Maildir only). |
| `%g` | GUID (128-bit hex, dbox/mdbox). |
| `%m` | MD5 of the filename. |

Common presets:

| Format | Result example | Compatible with |
|:---|:---|:---|
| `%u.%v` | `1234.5678` | yarilo default |
| `%08Xu%08Xv` | `000004D2000016C2` | the reference |
| `%f` | `1700000000.M123P456.host:2,S` | Courier (Maildir) |

---

## Soft-delete (flag mode)

When `pop3_delete_type: flag`, messages deleted by a POP3 client are not removed from disk. Instead the flag defined by `pop3_deleted_flag` is set, and the message remains visible in IMAP. This allows users to switch between POP3 and IMAP without losing mail.

```yaml
protocol:
  pop3:
    pop3_delete_type: flag
    pop3_deleted_flag: "$POP3Deleted"
```

---

## Migration from other servers

To migrate users from Courier, qmail, or cPanel without changing UIDL values (which would cause POP3 clients to re-download all mail):

```yaml
protocol:
  pop3:
    pop3_reuse_xuidl: true
    pop3_uidl_format: "%f"      # match the source server's format
    pop3_uidl_duplicates: rename
```

---

## UIDL stability (`pop3_save_uidl`)

By default, UIDLs are computed fresh each session from `pop3_uidl_format`. If the index is rebuilt or a backend migration changes filenames, UIDLs may change — causing POP3 clients to re-download mail they already have.

Enable `pop3_save_uidl` to persist the computed UIDL for each message:

```yaml
protocol:
  pop3:
    pop3_save_uidl: true
```

UIDLs are saved to `$HOME/.<folder>/pop3.uidl` (one `uid\tuidl` pair per line) at the end of each session via atomic write. On the next session, saved values are loaded and used for existing UIDs; new messages get fresh UIDLs that are then persisted.

Priority when multiple sources exist: `pop3_reuse_xuidl` header → saved index entry → format-computed value.

---

## Session locking (`pop3_lock_session`)

In multi-replica deployments (yarilo-pop3 `replicas > 1` on a shared PVC), two pods could serve the same user simultaneously — one via POP3 and one via IMAP. Enable `pop3_lock_session` to prevent this:

```yaml
protocol:
  pop3:
    pop3_lock_session: true
```

After login, the session creates `$HOME/yarilo-pop3-session.lock`. A second session for the same user on any pod that shares the filesystem will be rejected with `-ERR mailbox already in use`. The lock is released when the session ends (QUIT or disconnect). Locks older than 10 minutes (2× the idle timeout) are automatically treated as stale and stolen.

| Helm value | Config key | Default | Description |
|:---|:---|:---|:---|
| `protocol.pop3.saveUIDL` | `pop3_save_uidl` | `false` | Persist UIDLs to index file. |
| `protocol.pop3.lockSession` | `pop3_lock_session` | `false` | Dotlock against concurrent access. |
