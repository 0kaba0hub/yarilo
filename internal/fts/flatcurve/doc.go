// Package flatcurve is the Xapian-backed FTS engine, on-disk compatible
// with the flatcurve index format: per-mailbox "fts-flatcurve" directory
// of Xapian shards (current.### write shard, index.### sealed shards),
// docid == IMAP UID, term prefixes A (all headers), H<NAME> (indexed
// header), B (header existence). See https://doc.yarilomail.org/FTS §9.1.
//
// Requires cgo + libxapian; compiled only with the "flatcurve" build tag
// (yarilo-fts binary). Raw Xapian access goes through
// github.com/0kaba0hub/go-xapian.
package flatcurve
