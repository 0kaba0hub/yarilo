package mailindex

import (
	"encoding/binary"
	"fmt"
)

// RecordMinSize is the on-disk size of the base record (uid + flags)
// before any extension bytes are appended. Equals sizeof(uint32) +
// sizeof(uint8).
const RecordMinSize = 5

// MailFlag bits live in Record.Flags.
type MailFlag uint8

// IMAP mail-flags subset stored in the base record. Order/values
// match the on-disk encoding the canonical reader expects.
const (
	FlagAnswered MailFlag = 0x01
	FlagFlagged  MailFlag = 0x02
	FlagDeleted  MailFlag = 0x04
	FlagSeen     MailFlag = 0x08
	FlagDraft    MailFlag = 0x10
	// FlagUnused (0x20) is reserved and always zero on write —
	// pre-format-7 used it for MAIL_RECENT.
	FlagUnused MailFlag = 0x20
	// FlagBackend (0x40) is a per-driver scratch bit. The
	// mail-index layer does not interpret it.
	FlagBackend MailFlag = 0x40
	// FlagDirty (0x80) marks a record whose flag bits have not
	// yet been flushed to the storage backend.
	FlagDirty MailFlag = 0x80
)

// FlagsMask is the set of flags the mail-index treats as
// IMAP-visible (matches MAIL_INDEX_FLAGS_MASK). Backend/dirty
// bits are NOT in this mask — they're per-driver internal state.
const FlagsMask MailFlag = FlagAnswered | FlagFlagged | FlagDeleted | FlagSeen | FlagDraft

// Record is one logical row in the index. Base fields are
// always present; extension bytes appear in the on-disk record
// according to the per-extension (record_offset, record_size)
// pinned at EXT_INTRO time — see Extension and RecordLayout.
//
// Ext is keyed by extension name; each value is exactly
// extension.RecordSize bytes (or nil to mean "extension
// registered but no per-record bytes for this record yet" — the
// reader fills missing entries with zero-byte slices of the
// right length).
type Record struct {
	UID   uint32
	Flags MailFlag
	Ext   map[string][]byte
}

// EncodeRecord writes one record into buf according to layout.
// buf MUST be exactly layout.RecordSize bytes long; the caller
// is responsible for sizing the destination buffer (typical
// pattern: pre-allocate a flat []byte of N*RecordSize for all
// records, then slice into chunks for each Record).
//
// Missing extension bytes (Ext[name] == nil) are written as
// zeros — matches the canonical behaviour where a record
// predates the extension's introduction.
func EncodeRecord(buf []byte, layout RecordLayout, rec *Record) error {
	if uint32(len(buf)) != layout.RecordSize {
		return fmt.Errorf("mailindex: encode record: buf %d, layout %d: %w", len(buf), layout.RecordSize, ErrCorrupted)
	}
	for i := range buf {
		buf[i] = 0
	}
	binary.LittleEndian.PutUint32(buf[0:], rec.UID)
	buf[4] = uint8(rec.Flags)
	for _, ext := range layout.Extensions {
		if ext.RecordSize == 0 {
			continue
		}
		data, ok := rec.Ext[ext.Name]
		if !ok || len(data) == 0 {
			continue
		}
		if uint16(len(data)) != ext.RecordSize {
			return fmt.Errorf("mailindex: encode record: ext %q has %d bytes, want %d: %w",
				ext.Name, len(data), ext.RecordSize, ErrCorrupted)
		}
		copy(buf[ext.RecordOffset:ext.RecordOffset+ext.RecordSize], data)
	}
	return nil
}

// DecodeRecord parses one record from buf using layout. buf MUST
// be exactly layout.RecordSize bytes long.
//
// The returned Record's Ext is populated for every extension
// declared in the layout, with each value being a fresh
// extension.RecordSize-byte slice (not a sub-slice of buf — the
// caller can mutate either independently).
func DecodeRecord(buf []byte, layout RecordLayout) (Record, error) {
	if uint32(len(buf)) != layout.RecordSize {
		return Record{}, fmt.Errorf("mailindex: decode record: buf %d, layout %d: %w", len(buf), layout.RecordSize, ErrCorrupted)
	}
	rec := Record{
		UID:   binary.LittleEndian.Uint32(buf[0:]),
		Flags: MailFlag(buf[4]),
	}
	if len(layout.Extensions) > 0 {
		rec.Ext = make(map[string][]byte, len(layout.Extensions))
		for _, ext := range layout.Extensions {
			if ext.RecordSize == 0 {
				continue
			}
			data := make([]byte, ext.RecordSize)
			copy(data, buf[ext.RecordOffset:ext.RecordOffset+ext.RecordSize])
			rec.Ext[ext.Name] = data
		}
	}
	return rec, nil
}
