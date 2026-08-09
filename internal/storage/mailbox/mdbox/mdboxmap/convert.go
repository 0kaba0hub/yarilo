package mdboxmap

import (
	"errors"
	"log/slog"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// Conversion between base formats, in whichever direction mdbox_map_format asks
// for. It exists because the map is not derivable state: map_uid lives only
// here, the dbox trailer does not carry it, and every per-folder index
// references messages by it. Rebuilding from the storage files would mint new
// identities and orphan every folder record, so the old bytes are read rather
// than discarded — in both directions, since the format on disk is what decides
// whether a rolled-back binary can still open the map.

// isForeignBase reports whether err says the file does not carry the v2 magic
// at all — the case worth trying to read as v1. A file that does carry the magic
// under a version this binary does not know is refused, never converted and
// never rebuilt: a newer version's records are not ours to reinterpret.
func isForeignBase(err error) bool {
	var e errUnknownBaseVersion
	return errors.As(err, &e) && !e.magicOK
}

// convertToConfiguredLocked rewrites the base in the configured format when the
// file on disk is in the other one. A no-op when they already agree.
//
// Both writers are atomic (.tmp + rename), so the old file stays readable until
// the new one is complete: an interrupted conversion leaves the previous format
// in place and the next open converts again. The log is left alone and recorded
// as folded up to where this read got, so a transaction appended while the
// conversion ran is still applied afterwards.
func (m *Map) convertToConfiguredLocked() error {
	if m.onDisk == m.format {
		return nil
	}
	from, to := m.onDisk, m.format

	seq, _, err := m.logIdentityLocked()
	if err != nil {
		return err
	}
	applied, rerr := m.replayLogInnerLocked(mailindex.LogHeaderSize)
	if rerr != nil && !errors.Is(rerr, errLogIndexMismatch) {
		return rerr
	}
	if seq == 0 {
		// Replaying a log that is not there returns the offset it was given;
		// carrying that over would leave the handle claiming to have applied
		// bytes of a file that does not exist.
		applied = 0
	}
	// The log stays where it is and keeps being appended to, so the new base
	// records it as folded up to here. Its own lineage must differ from the
	// log's, or a later reader would take that log for one written after this
	// base and apply its transactions a second time.
	m.foldedLineage, m.foldedOffset = seq, applied
	m.lineage = seq + 1
	m.logLineage = seq
	m.logSize = applied

	if err := m.writeBaseLocked(); err != nil {
		return err
	}
	slog.Warn("mdbox: map index converted between base formats",
		"user", m.username, "path", m.path, "from", string(from), "to", string(to),
		"records", m.st.count(), "next_map_uid", m.nextMapUID)
	return nil
}
