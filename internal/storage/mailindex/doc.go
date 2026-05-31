// Package mailindex is a wire-compatible Go implementation of
// the mail-index binary format (major=7 minor=3).
//
// Three on-disk files use this format and are all encoded by this
// package:
//
//   - dovecot.index           — base index (header + records)
//   - dovecot.index.log       — transaction log
//   - dovecot.map.index       — mdbox global map (same format,
//     different registered extensions)
//
// The package is intentionally low-level: it knows about bytes,
// records, extensions, and transaction types — nothing about
// mailboxes, IMAP, or storage drivers. Higher layers
// (internal/storage/index/file for per-folder index;
// internal/storage/mailbox/mdbox/mdboxmap for mdbox map) build
// their semantics on top.
//
// Endianness: the wire format is little-endian on every platform
// yarilo supports (linux/amd64). compat_flags.0
// (LITTLE_ENDIAN) is always set; readers validate it on Open.
// Big-endian indexes are rejected.
package mailindex
