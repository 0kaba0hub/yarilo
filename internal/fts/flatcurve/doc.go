// Package flatcurve is the Xapian-backed FTS engine, byte-compatible with
// Dovecot 2.4's fts-flatcurve on-disk format: a per-mailbox "fts-flatcurve"
// directory of Xapian shards (current.### write shard, index.### sealed
// shards), docid == IMAP UID, term prefixes A (all headers), H<NAME>
// (indexed header) and B (header existence). See docs/FTS.md §9.1.
//
// The engine requires cgo + libxapian and is only compiled with the
// "flatcurve" build tag; without the tag this package contains no engine so
// the default pure-Go build is unaffected. Only the yarilo-fts binary is
// built with the tag.
package flatcurve
