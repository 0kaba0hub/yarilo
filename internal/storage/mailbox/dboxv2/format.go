// Package dboxv2 implements the single-message dbox (sdbox) storage
// driver, replacing yarilo's pre-Phase-3 internal/storage/mailbox/dbox
// implementation.
//
// On-disk layout:
//
//	<home>/sdbox/
//	  control/yarilo-uidvalidity      ← per-user uidvalidity counter
//	  mailboxes/<folder>/dbox-Mails/
//	    u.<UID>                        ← one file per message (decimal UID)
//	    yarilo.index*                 ← per-folder fileindex
//	    .temp.<sec>.P<pid>Q<seq>M<usec>.<host> ← in-flight saves
//
// File format (per the internal docs):
//
//	<file header line, ASCII>          "2 M20 C<create_stamp_hex>\n"
//	<dbox_message_header, 32 bytes>    magic + type + UID-slot (v2: spaces) + size
//	<body, message_size bytes>         CRLF-terminated lines
//	<dbox_metadata_header>             "\n\001\003\n"
//	<key><value>\n  ...                G, R, Z?, V, P?, O?, B?, X?
//	<empty line "\n">                  metadata terminator
//
// Save uses a two-phase pattern: Save writes to a .temp.* file and
// returns its name; the caller assigns a UID via the fileindex and
// then calls AssignUID, which renames the temp file to its final
// u.<UID> name. This gives a crash-safe atomic publish — a crash
// before AssignUID leaves only the temp file on disk; the periodic
// orphan cleanup ages those out.
package dboxv2

import (
	"encoding/hex"
	"fmt"
	"io"
)

// File-format constants (per the dbox spec).
const (
	// dboxVersion identifies the on-disk format. We always write
	// version 2; readers tolerate higher minor versions transparently.
	dboxVersion = 2

	// magicPre opens every message header.
	magicPre = "\x01\x02"
	// magicPost opens the metadata block.
	magicPost = "\n\x01\x03\n"

	// messageHeaderSize is the fixed on-disk size of
	// struct dbox_message_header at format version 2.
	messageHeaderSize = 32

	// messageType is the single value 'N' used today; future
	// extensions may add other types via the canonical reader.
	messageType byte = 'N'
)

// Metadata key bytes that the canonical reader recognises.
//
// Uppercase only — lowercase reserved for future use per
// the spec. Each value byte is followed by its ASCII value
// (typically hex digits or, for G, a 32-char lowercase GUID).
const (
	metaKeyGUID         byte = 'G' // 32 lowercase hex digits
	metaKeyReceived     byte = 'R' // creation timestamp (unix, hex)
	metaKeyPhysicalSize byte = 'Z' // body byte count, hex (omitted when equal to header size)
	metaKeyVirtualSize  byte = 'V' // CRLF-counted virtual size, hex
	metaKeyPOP3UIDL     byte = 'P' // optional POP3 UIDL override
	metaKeyPOP3Order    byte = 'O' // optional POP3 ordering hint
	metaKeyOrigMailbox  byte = 'B' // original mailbox name (rebuild hint)
	metaKeyExtRefs      byte = 'X' // external attachment refs
)

// messageHeader is the parsed 32-byte per-message header.
// Wire layout (every field is ASCII, encoded as documented):
//
//	offset 0   2   magic_pre        "\x01\x02"
//	offset 2   1   type             'N'
//	offset 3   1   space            ' '
//	offset 4   8   oldv1_uid_hex    8 spaces (v2 ignores; legacy v1 wrote a UID)
//	offset 12  1   space            ' '
//	offset 13  16  msg_size_hex     16 lowercase hex digits (body byte count)
//	offset 29  2   space-pad        "  "
//	offset 31  1   save_lf          '\n'
type messageHeader struct {
	Size uint64 // body length in bytes
}

// encodeMessageHeader returns the 32-byte on-disk image of h.
//
// The oldv1_uid_hex slot is filled with spaces — per the v2 spec.
// Padding bytes after msg_size_hex are spaces; the trailing byte is
// '\n' so a quick `head -c 32` on a saved file is human-readable.
func encodeMessageHeader(h messageHeader) []byte {
	buf := make([]byte, messageHeaderSize)
	for i := range buf {
		buf[i] = ' '
	}
	buf[0] = magicPre[0]
	buf[1] = magicPre[1]
	buf[2] = messageType
	// buf[3]  = ' '
	// buf[4..11] = "        " (v2 oldv1_uid_hex slot — always spaces)
	// buf[12] = ' '
	hexSize := fmt.Sprintf("%016x", h.Size)
	copy(buf[13:29], hexSize)
	// buf[29..30] = "  "
	buf[31] = '\n'
	return buf
}

// decodeMessageHeader parses the 32-byte header from b. Returns
// an error when magic or layout is wrong.
func decodeMessageHeader(b []byte) (messageHeader, error) {
	if len(b) < messageHeaderSize {
		return messageHeader{}, fmt.Errorf("dboxv2: message header %d bytes (<%d)", len(b), messageHeaderSize)
	}
	if b[0] != magicPre[0] || b[1] != magicPre[1] {
		return messageHeader{}, fmt.Errorf("dboxv2: bad message magic at offset 0")
	}
	if b[2] != messageType {
		return messageHeader{}, fmt.Errorf("dboxv2: unknown message type %q", b[2])
	}
	if b[31] != '\n' {
		return messageHeader{}, fmt.Errorf("dboxv2: missing trailing newline at offset 31")
	}
	hexBytes := b[13:29]
	var size uint64
	if _, err := fmt.Sscanf(string(hexBytes), "%016x", &size); err != nil {
		return messageHeader{}, fmt.Errorf("dboxv2: parse size hex %q: %w", hexBytes, err)
	}
	return messageHeader{Size: size}, nil
}

// encodeFileHeaderLine returns the leading "2 M20 C<stamp>\n"
// text line that every dbox file carries. stamp is the create
// timestamp in lowercase hex.
func encodeFileHeaderLine(createStamp uint32) []byte {
	return []byte(fmt.Sprintf("%d M%x C%x\n", dboxVersion, messageHeaderSize, createStamp))
}

// metadataEntry is one key/value pair from the trailer block.
type metadataEntry struct {
	Key   byte
	Value string
}

// encodeMetadataBlock returns the bytes from magic_post (\n\x01\x03\n)
// through the trailing empty newline that terminates the block.
//
// Entries are written in the order supplied; the spec does not
// require any particular order but readers tolerate any sequence.
func encodeMetadataBlock(entries []metadataEntry) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, magicPost...)
	for _, e := range entries {
		buf = append(buf, e.Key)
		buf = append(buf, e.Value...)
		buf = append(buf, '\n')
	}
	buf = append(buf, '\n')
	return buf
}

// decodeMetadataBlock parses entries from r until it sees the
// terminating blank line. Returns ErrUnexpectedEOF when the trailer
// is truncated. Unknown keys are preserved so the caller can carry
// them forward through a rebuild without dropping data.
func decodeMetadataBlock(r io.Reader) ([]metadataEntry, error) {
	br := newByteReader(r)
	// Read magic_post: "\n\x01\x03\n"
	want := []byte(magicPost)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(br, got); err != nil {
		return nil, fmt.Errorf("dboxv2: read magic_post: %w", err)
	}
	for i, b := range want {
		if got[i] != b {
			return nil, fmt.Errorf("dboxv2: bad magic_post byte %d: 0x%02x want 0x%02x", i, got[i], b)
		}
	}
	var out []metadataEntry
	for {
		key, err := br.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("dboxv2: read meta key: %w", err)
		}
		if key == '\n' {
			return out, nil
		}
		// Read up to and including LF, drop the LF.
		val, err := br.ReadUntil('\n')
		if err != nil {
			return nil, fmt.Errorf("dboxv2: read meta value for key %q: %w", key, err)
		}
		out = append(out, metadataEntry{Key: key, Value: string(val[:len(val)-1])})
	}
}

// guidHex returns a 32-character lowercase hex encoding of a
// 16-byte GUID — the format the canonical reader expects in the
// G metadata entry.
func guidHex(g [16]byte) string { return hex.EncodeToString(g[:]) }
