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

// MailboxKey returns the lock key for a specific mailbox folder.
// Acquire after IndexKey, before DeliverKey.
func MailboxKey(user, folder string) string { return "mbox:" + user + ":" + folder }

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
