package mdboxmap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// The map keeps an append-only transaction log (yarilo.map.index.log) beside the
// base index. Deliveries append TxAppend records to the log instead of rewriting
// the whole base; a reader picks up a sibling process's deliveries incrementally
// by replaying the log tail past its last-applied offset (see reloadLocked). The
// base is rewritten (and the log dropped) only on compaction and on the rarer
// full-state mutations (refcount, purge, file-id allocation).

// mapLogCompactBytes bounds the append log: once it grows past this, the next
// append folds it into the base and truncates the log so replay stays cheap.
const mapLogCompactBytes = 256 << 10

var errLogIndexMismatch = errors.New("mdboxmap: log IndexID mismatch")

func (m *Map) logPath() string { return m.path + ".log" }

// appendLogLocked appends recs to the map log as a single TxAppend transaction.
// Caller holds m.mu and the cross-process map lock.
func (m *Map) appendLogLocked(recs []*mailindex.Record) error {
	if len(recs) == 0 {
		return nil
	}
	layout, err := mailindex.ComputeRecordLayout(m.f.Extensions)
	if err != nil {
		return fmt.Errorf("mdboxmap/log: layout: %w", err)
	}
	payload, err := mailindex.EncodeTxAppendPayload(layout, recs)
	if err != nil {
		return fmt.Errorf("mdboxmap/log: encode: %w", err)
	}

	f, err := os.OpenFile(m.logPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("mdboxmap/log: open: %w", err)
	}
	defer f.Close()
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		hdr := mailindex.NewLogHeader(m.f.Header.IndexID, 1, uint32(time.Now().Unix()))
		if err := hdr.Encode(f); err != nil {
			return fmt.Errorf("mdboxmap/log: header: %w", err)
		}
	}
	rec, err := encMapLogRec(mailindex.TxTypeAppend, payload)
	if err != nil {
		return fmt.Errorf("mdboxmap/log: frame: %w", err)
	}
	if _, err := f.Write(rec); err != nil {
		return fmt.Errorf("mdboxmap/log: write: %w", err)
	}
	if st, err := os.Stat(m.logPath()); err == nil {
		m.logSize = st.Size()
	}
	return nil
}

// appendRefcountLogLocked records refcount deltas as one EXT_ATOMIC_INC
// transaction. Deltas, not absolute values: an absolute write is only safe
// while every writer holds the map lock and has reloaded first, and a future
// path that does neither would silently lose an update -- where a lost
// decrement leaks a file and a lost increment lets purge delete a message that
// is still referenced.
//
// The reference format applies the increment to the last EXT_INTRO'd
// extension. This log introduces none, so the map replay applies it to the
// refcount extension by convention; both sides live in this package.
func (m *Map) appendRefcountLogLocked(deltas []mailindex.TxExtAtomicInc) error {
	if len(deltas) == 0 {
		return nil
	}
	f, err := os.OpenFile(m.logPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("mdboxmap/log: open: %w", err)
	}
	defer f.Close()
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		hdr := mailindex.NewLogHeader(m.f.Header.IndexID, 1, uint32(time.Now().Unix()))
		if err := hdr.Encode(f); err != nil {
			return fmt.Errorf("mdboxmap/log: header: %w", err)
		}
	}
	rec, err := encMapLogRec(mailindex.TxTypeExtAtomicInc, mailindex.EncodeTxExtAtomicIncPayload(deltas))
	if err != nil {
		return fmt.Errorf("mdboxmap/log: frame: %w", err)
	}
	if _, err := f.Write(rec); err != nil {
		return fmt.Errorf("mdboxmap/log: write: %w", err)
	}
	if st, err := os.Stat(m.logPath()); err == nil {
		m.logSize = st.Size()
	}
	// Same threshold the append path uses: the log is bounded, not unbounded.
	if m.logSize > mapLogCompactBytes {
		return m.flushLocked()
	}
	return nil
}

// commitAppendLocked persists the last n freshly-appended records: it writes
// them to the append log (the incremental hot path) and folds the log into the
// base once it outgrows mapLogCompactBytes. Caller holds m.mu + the map lock.
func (m *Map) commitAppendLocked(n int) error {
	if n <= 0 {
		return nil
	}
	newRecs := m.f.Records[len(m.f.Records)-n:]
	if err := m.appendLogLocked(newRecs); err != nil {
		return err
	}
	if m.logSize > mapLogCompactBytes {
		return m.flushLocked()
	}
	return nil
}

// encMapLogRec frames one transaction record: an 8-byte tx header followed by
// the payload. The record's total size must be 4-byte aligned (the log's framed
// size encoding requires it), so the payload is zero-padded up to a 4-byte
// boundary. A reader iterates whole records (stride = RecordSize) and stops
// before the trailing pad, so the padding is transparent.
func encMapLogRec(txType mailindex.TxType, payload []byte) ([]byte, error) {
	if pad := (4 - len(payload)%4) % 4; pad > 0 {
		payload = append(append([]byte(nil), payload...), make([]byte, pad)...)
	}
	hdrBuf := make([]byte, 8)
	if err := mailindex.EncodeTxHeader(hdrBuf, mailindex.TxHeader{
		Size: uint32(8 + len(payload)),
		Type: mailindex.TxTypeFlags(txType),
	}); err != nil {
		return nil, err
	}
	out := make([]byte, 8+len(payload))
	copy(out, hdrBuf)
	copy(out[8:], payload)
	return out, nil
}

// replayLogLocked reads TxAppend records from the log starting at fromOffset and
// appends any records not already present in m.f. Returns the new applied byte
// offset. A torn tail (crash mid-write) stops replay cleanly without consuming
// the partial record. Caller holds m.mu; caller re-runs reindex() afterwards.
// replayLogLocked reads the log from fromOffset and folds it in. Timed as the
// "replay" part of a freshness check.
func (m *Map) replayLogLocked(fromOffset int64) (off int64, err error) {
	start := time.Now()
	defer func() {
		metricMapReloadPart.WithLabelValues("replay").Observe(time.Since(start).Seconds())
	}()
	return m.replayLogInnerLocked(fromOffset)
}

func (m *Map) replayLogInnerLocked(fromOffset int64) (int64, error) {
	f, err := os.Open(m.logPath())
	if errors.Is(err, os.ErrNotExist) {
		return fromOffset, nil
	}
	if err != nil {
		return fromOffset, fmt.Errorf("mdboxmap/log: open: %w", err)
	}
	defer f.Close()

	lh, err := mailindex.DecodeLogHeader(f)
	if err != nil {
		return fromOffset, nil // empty or unreadable log
	}
	if lh.IndexID != m.f.Header.IndexID {
		return fromOffset, errLogIndexMismatch
	}
	pos := int64(mailindex.LogHeaderSize)
	if fromOffset > pos {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return fromOffset, fmt.Errorf("mdboxmap/log: seek: %w", err)
		}
		pos = fromOffset
	}
	layout, err := mailindex.ComputeRecordLayout(m.f.Extensions)
	if err != nil {
		return fromOffset, fmt.Errorf("mdboxmap/log: layout: %w", err)
	}
	stride := int(layout.RecordSize)
	if stride == 0 {
		return pos, nil
	}
	// UID -> record, built here rather than from m.byMapUID: that index is
	// rebuilt after the replay, so during it it still describes the previous
	// state -- and a delta applied through it would land nowhere.
	existing := make(map[uint32]*mailindex.Record, len(m.f.Records))
	for _, r := range m.f.Records {
		existing[r.UID] = r
	}
	hdrBuf := make([]byte, 8)
	for {
		_, rerr := io.ReadFull(f, hdrBuf)
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		} else if rerr != nil {
			return pos, fmt.Errorf("mdboxmap/log: read hdr: %w", rerr)
		}
		txHdr, derr := mailindex.DecodeTxHeader(hdrBuf)
		if derr != nil {
			break // torn write
		}
		payloadLen := int(txHdr.Size) - 8
		if payloadLen < 0 || payloadLen > 1<<24 {
			break
		}
		payload := make([]byte, payloadLen)
		if _, rerr := io.ReadFull(f, payload); rerr != nil {
			break // torn tail — do not advance pos past the partial record
		}
		pos += int64(8 + payloadLen)
		if txHdr.Type.Kind() == mailindex.TxTypeExtAtomicInc {
			applyRefcountDeltas(existing, payload)
			continue
		}
		if txHdr.Type.Kind() != mailindex.TxTypeAppend {
			continue
		}
		for i := 0; i+stride <= len(payload); i += stride {
			rec, rderr := mailindex.DecodeRecord(payload[i:i+stride], layout)
			if rderr != nil {
				break
			}
			if _, dup := existing[rec.UID]; dup {
				continue
			}
			rp := rec
			m.f.Records = append(m.f.Records, &rp)
			existing[rp.UID] = &rp
		}
	}
	return pos, nil
}

// applyRefcountDeltas folds one EXT_ATOMIC_INC payload into the records the
// replay has built so far.
//
// A delta naming an unknown UID is skipped, and the two directions are not
// equally forgiving: a skipped decrement only keeps a dead file alive, while a
// skipped increment lets purge delete a message that is still referenced. This
// is safe on one invariant -- a record is durable before any delta against it,
// since the append is logged (or flushed) before the refcount that follows it.
// If that ever stops holding, this skip is where the breakage would hide.
func applyRefcountDeltas(byUID map[uint32]*mailindex.Record, payload []byte) {
	for _, d := range mailindex.DecodeTxExtAtomicIncPayload(payload) {
		rec, ok := byUID[d.UID]
		if !ok {
			continue
		}
		rec.Ext[extRef] = encodeRefExt(clampRef(int32(decodeRefExt(rec.Ext[extRef])) + d.Diff))
	}
}

// clampRef keeps a refcount inside the 16 bits the extension carries. The
// floor matters: a count driven below zero by a replayed decrement would wrap
// to a huge number and keep a dead file alive forever, and the ceiling is the
// format's.
func clampRef(v int32) uint16 {
	switch {
	case v < 0:
		return 0
	case v > 0xFFFF:
		return 0xFFFF
	default:
		return uint16(v)
	}
}
