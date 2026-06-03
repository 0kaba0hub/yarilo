# mdbox Alt Storage (Cold Tiering)

yarilo mdbox supports moving messages to a secondary (cold) storage
tier — functionally equivalent to Dovecot's `mail_alt_path` +
`doveadm altmove`. Messages moved to alt storage remain fully
accessible; the Fetch path transparently falls back to the alt
directory when a file is not found in primary.

## Configuration

```yaml
storage:
  mailbox: mdbox
  mail_home_template: "%d/%n"
  mdbox_alt_storage_path: "/mnt/cold/%d/%n"   # "" = disabled (default)
```

`mdbox_alt_storage_path` supports the same template variables as
`mail_home_template`:

| Variable | Expands to |
|:---|:---|
| `%u` | Full username (`alice@example.com`) |
| `%Lu` | Lowercased full username |
| `%n` | Local part (`alice`) |
| `%Ln` | Lowercased local part |
| `%d` | Domain (`example.com`) |
| `%Ld` | Lowercased domain |

The alt directory mirrors the primary mdbox layout:

```
/mnt/cold/example.com/alice/
  storage/
    m.1001, m.1002, ...    ← moved m.<N> files (same naming scheme)
```

File IDs are global across both tiers — the same `m.<N>` ID never
exists in both primary and alt simultaneously.

## Helm

```yaml
# values.yaml
imap:
  storage:
    mdboxAltStoragePath: "/mnt/cold/%d/%n"
```

A separate PVC (or NFS export) should be mounted at the cold path —
typically a cheaper storage class (HDD-backed, object-gateway, etc.).

## Triggering a move

```sh
# Move all messages older than 2025-01-01 to cold storage:
yarilo-admin backend mdbox altmove alice@example.com \
  --before 2025-01-01T00:00:00Z

# Move all messages (no date filter):
yarilo-admin backend mdbox altmove alice@example.com

# Move back from cold to primary (reverse):
yarilo-admin backend mdbox altmove alice@example.com \
  --before 2025-01-01T00:00:00Z \
  --reverse
```

`--before` accepts RFC 3339 timestamps. Messages whose `InternalDate`
(the `R` field in the dbox v2 trailer — set when the message was first
saved) is strictly before the cutoff are eligible.

## Operational notes

### Fetch transparency

When a session fetches a message whose primary `m.<N>` file has been
moved, the driver automatically opens the alt file. The IMAP client
sees no difference. The fallback only fires on `ENOENT` — any other
open error (permissions, I/O) is returned as-is.

### Refcount and COPY

O(1) IMAP COPY works identically across both tiers: `Copy()` only
bumps the refcount; the copied map_uid continues to point at whichever
tier holds the physical body. A subsequent `altmove` on the copied
map_uid moves both the original and the copy together (they share the
same map_uid).

### Partial file moves

When a source `m.<N>` contains both eligible and ineligible records,
`altmove` splits the file: eligible records go to a new alt `m.<N>`,
ineligible records stay in a new primary `m.<N>`. Two new files are
created; the old source is unlinked. The map index is updated
atomically.

### Interplay with Purge

Run `purge` before `altmove` to avoid compacting zero-ref records
into the alt tier unnecessarily:

```sh
yarilo-admin backend mdbox purge alice@example.com
yarilo-admin backend mdbox altmove alice@example.com --before 2025-01-01T00:00:00Z
```

### Automation via CronJob

```yaml
# k8s CronJob — monthly cold-tier sweep for messages older than 90 days
apiVersion: batch/v1
kind: CronJob
metadata:
  name: mdbox-altmove
spec:
  schedule: "0 2 1 * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: altmove
            image: ghcr.io/0kaba0hub/yarilo:latest
            command:
            - yarilo-admin
            - backend
            - mdbox
            - altmove
            - alice@example.com
            - --before
            - "{{ now | date \"2006-01-02T15:04:05Z\" | dateModify \"-2160h\" }}"
          restartPolicy: OnFailure
```

In practice, drive the user list from the SQL passdb iterate_query
and loop per user.

## Wire compatibility with Dovecot

The on-disk format of moved `m.<N>` files is identical to primary
files — same dbox v2 record layout. Dovecot's `mail_alt_path` and
yarilo's `mdbox_alt_storage_path` are interchangeable at the
filesystem level, enabling live migration between the two servers
without data conversion.
