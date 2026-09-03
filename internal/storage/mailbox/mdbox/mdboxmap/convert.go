package mdboxmap

import (
	"errors"
	"log/slog"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// Conversion between base formats, both directions: the map is not derivable, and
// the format on disk decides what a rolled-back binary can open.

// isForeignBase reports no v2 magic at all, worth trying as v1. Magic under an
// unknown version is refused: those records are not ours to reinterpret.
func isForeignBase(err error) bool {
	var e errUnknownBaseVersion
	return errors.As(err, &e) && !e.magicOK
}

// convertToConfiguredLocked rewrites the base into the configured format. Both
// writers are atomic, so an interrupted conversion leaves the old one readable.
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
		// Replaying an absent log returns the offset it was given, which would
		// claim bytes of a file that does not exist.
		applied = 0
	}
	// The log is still appended to, so the base records it folded up to here.
	// Their lineages must differ, or a later reader applies it twice.
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
