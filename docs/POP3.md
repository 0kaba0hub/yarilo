# POP3 configuration

Yarilo implements POP3 (RFC 1939) with STLS (RFC 2595), UIDL, CAPA, and XCLIENT.

Listeners: `services.pop3s` (implicit TLS, port 995) and `services.pop3` (STARTTLS, port 110).
See [SERVICES.md](SERVICES.md) for listener-level settings (port, ssl_mode, haproxy_protocol, etc.).

---

## `protocol.pop3`

Protocol-level behaviour, shared across both POP3 listeners.

| Key | Default | Description |
|:---|:---|:---|
| `pop3_no_flag_updates` | `false` | `false` = set `\Seen` on RETR'd messages at QUIT (Dovecot default). `true` = no flag changes on retrieval. |
| `pop3_reuse_xuidl` | `false` | Use the `X-UIDL` message header as the UIDL value. Enables migration from Courier / qmail / cPanel without UIDL changes. |
| `pop3_uidl_format` | `%u.%v` | UIDL format string. See variables below. |
| `pop3_uidl_duplicates` | `rename` | `allow` = emit duplicate UIDLs as-is. `rename` = append `-N` suffix to guarantee uniqueness. |
| `pop3_enable_last` | `false` | Advertise and handle the `LAST` command (RFC 1460). |
| `pop3_delete_type` | `expunge` | `expunge` = remove message from disk at QUIT. `flag` = set `pop3_deleted_flag` (soft delete, keeps message in IMAP). |
| `pop3_deleted_flag` | `""` | IMAP flag to set when `pop3_delete_type: flag`. Example: `$POP3Deleted`. |

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
| `%08Xu%08Xv` | `000004D2000016C2` | Dovecot |
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
