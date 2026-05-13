# JMAP configuration

> **Status: planned (Phase 5)**

JMAP (RFC 8620 / RFC 8621) support is planned for a future release. This document describes the intended configuration structure.

---

## Planned listeners

| Service key | Port | Protocol |
|:---|:---|:---|
| `jmap` | `8443` | HTTPS (JSON API + WebSocket push) |

---

## Planned `protocol.jmap`

| Key | Default | Description |
|:---|:---|:---|
| `base_url` | — | Public HTTPS base URL served to clients (used in `/.well-known/jmap`). |
| `max_concurrent_requests` | `10` | Max simultaneous JMAP API calls per session. |
| `max_objects_in_get` | `500` | Max objects per `Foo/get` call. |
| `max_objects_in_set` | `500` | Max objects per `Foo/set` call. |
| `max_size_upload` | `41943040` | Max blob upload size in bytes. |
| `push_timeout` | `90` | WebSocket push connection timeout in seconds. |

---

## Planned JMAP capabilities

| Capability | RFC | Notes |
|:---|:---|:---|
| Core | RFC 8620 | Session, upload/download, event source. |
| Mail | RFC 8621 | Mailbox, Email, Thread, SearchSnippet, Identity, EmailSubmission, VacationResponse. |
| WebSocket push | RFC 8887 | Real-time state change notifications. |

Updates to this document will follow the Phase 5 implementation.
