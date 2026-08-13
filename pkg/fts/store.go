package fts

// IndexStore is where an engine's per-mailbox indexes live. It exists so
// fts_index_root selects an implementation instead of naming a path the engine
// joins itself: the engine asks for a mailbox's index and is told where it is,
// and everything the medium implies — how a directory is created, whether
// committing its metadata means anything — belongs to the store (#1053).
//
// The operations are the ones an engine performs today, no more: a second
// medium will bring its own additions, and guessing them now would be an
// interface written for a store nobody has.
type IndexStore interface {
	// Locate returns where a mailbox's index belongs. It touches nothing, so
	// it answers for an index that does not exist yet.
	Locate(user UserRef, mbox MailboxRef) string

	// Prepare returns the mailbox's index location, having adopted an index
	// left at a layout an earlier version used. It does not create anything:
	// an engine that only reads must not leave a directory behind.
	//
	// Best-effort on the adoption: a migration that fails leaves the older
	// index where it is and returns the current location, which the engine
	// fills by reindexing.
	Prepare(user UserRef, mbox MailboxRef) (string, error)

	// Create materialises the index location so the engine can write into it.
	Create(dir string) error

	// Sync commits the location's own metadata — the entries naming shards,
	// not their contents — where the medium makes that meaningful. On a medium
	// that commits metadata by protocol this is where the call is skipped
	// rather than issued as a no-op.
	Sync(dir string) error
}

// Layout is an engine's on-disk shape, which a store executes. The engine owns
// it — where its shards sit under a root, and which shapes earlier versions
// wrote — while the store owns the medium those paths live on. Keeping the two
// apart is what lets a second medium reuse the layout, and a second engine
// reuse the medium.
type Layout struct {
	// Dir names a mailbox's index under root.
	Dir func(root string, user UserRef, mbox MailboxRef) string
	// Legacy names the locations earlier versions used for the same mailbox,
	// newest first. A store adopts the first one it finds.
	Legacy func(root string, user UserRef, mbox MailboxRef) []string
}
