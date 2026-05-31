package mailindex

import "errors"

// Sentinel errors. Wrap with errors.Is at call sites.
var (
	// ErrShortRead indicates a truncated read of a fixed-size
	// structure (header, record, transaction frame). The file is
	// either being concurrently truncated or never had the full
	// structure written.
	ErrShortRead = errors.New("mailindex: short read")

	// ErrMajorMismatch is returned when an on-disk index has a
	// different major version than this package targets. Major
	// version is a hard reject — readers must drop the file and
	// (typically) trigger a rebuild from the underlying storage.
	ErrMajorMismatch = errors.New("mailindex: major version mismatch")

	// ErrEndian is returned when the on-disk compat_flags omits
	// the LITTLE_ENDIAN bit. This package writes little-endian
	// only and rejects big-endian files at read time — there is
	// no in-place conversion.
	ErrEndian = errors.New("mailindex: non-little-endian index not supported")

	// ErrCorrupted is returned when an internal invariant fails
	// while decoding (e.g. an extension claims a record offset
	// that overflows the record_size). The caller's typical
	// response is to set the on-disk HdrFlagFsckd and trigger a
	// driver-level rebuild.
	ErrCorrupted = errors.New("mailindex: file corrupted")

	// ErrUnknownExtension is returned when a transaction record
	// names an extension that was never introduced via EXT_INTRO
	// during the log replay.
	ErrUnknownExtension = errors.New("mailindex: unknown extension")

	// ErrUnknownTxType is returned when the log replay encounters
	// a transaction type bit this package does not implement.
	// Forward-compat strategy: log a warning, skip the record
	// using the size field, continue.
	ErrUnknownTxType = errors.New("mailindex: unknown transaction type")
)
