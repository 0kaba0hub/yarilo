package locks

// Resource-key constructors. Storage code MUST use these instead of building
// strings inline, so a future prefix change can be tracked through the
// compiler instead of grep. Naming follows the convention in
// docs/DEPLOYMENT.md §Deadlock prevention:
//
//	idx:<user>                  — per-user index lock (taken first)
//	mbox:<user>:<folder>        — per-mailbox lock (taken second)
//	deliver:<user>:<folder>     — per-mailbox delivery serialisation (last)
//
// Callers must always acquire in this order to avoid deadlocks.

// IndexKey returns the lock key for the per-user index.
// Acquire first in any multi-resource sequence.
func IndexKey(user string) string { return "idx:" + user }

// MdboxMapKey returns the lock key for the user-wide mdbox map
// (dovecot.map.index). The mdbox driver acquires this BEFORE any
// MailboxKey when allocating map_uids / file_ids; this is the
// strict map-then-folder order documented in
// docs/STORAGE-COMPLIANCE.md §4.2.
func MdboxMapKey(user string) string { return "mdboxmap:" + user }

// MailboxKey returns the lock key for a specific mailbox folder.
// Acquire after IndexKey, before DeliverKey.
func MailboxKey(user, folder string) string { return "mbox:" + user + ":" + folder }

// MailboxListKey returns the event-bus resource for a user's mailbox-list
// changes (create / delete / rename / subscribe). NOTIFY watchers subscribe to
// it to learn about mailboxes appearing or disappearing after NOTIFY SET. It is
// an event channel only, never taken as a lock.
func MailboxListKey(user string) string { return "mlist:" + user }

// DeliverKey returns the lock key for serialising delivery into a mailbox.
// Acquire last. LMTP delivery uses this in addition to MailboxKey when the
// fan-out path needs to serialise per-recipient writes against IMAP STORE.
func DeliverKey(user, folder string) string { return "deliver:" + user + ":" + folder }

// SieveScriptsKey returns the lock key for a user's Sieve script directory.
// Independent of mailbox keys — Sieve updates do not interleave with mail
// writes.
func SieveScriptsKey(user string) string { return "sieve:" + user }

// SubscriptionsKey returns the lock key for a user's mailbox-subscription
// file (LIST-EXTENDED §SELECT SUBSCRIBED / RETURN SUBSCRIBED). Independent
// of mailbox keys — subscribe is metadata, never racing with mail writes.
func SubscriptionsKey(user string) string { return "subs:" + user }

// ACLListKey returns the lock key for the per-namespace yarilo-acl-list
// global index — one file per namespace-root that mirrors which
// mailboxes have explicit ACLs. Scoped by home directory rather than
// username so two users accessing the same shared-namespace root
// serialise their index updates against each other. Independent of
// MailboxKey: per-mailbox ACL writes take MailboxKey, then briefly
// take ACLListKey to splice their change into the index.
func ACLListKey(home string) string { return "acllist:" + home }
