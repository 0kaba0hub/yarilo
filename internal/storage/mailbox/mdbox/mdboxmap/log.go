package mdboxmap

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/logrotate"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// The map keeps an append-only log beside the base: a delivery appends instead of
// rewriting, and a reader replays the tail past its last-applied offset. The
// log's layout is fixed here, not derived from the base.

// Rotation defaults, shared with the folder index (#1258). The floor exists
// because every open replays the tail at ~0.5us a byte: under the old flat
// threshold a legal tail cost ~136ms on every open.
var (
	defaultLogRotateMinSize = logrotate.MinSize
	defaultLogRotateMaxSize = logrotate.MaxSize
	defaultLogRotateMinAge  = logrotate.MinAge
)

var errLogIndexMismatch = errors.New("mdboxmap: log IndexID mismatch")

func (m *Map) logPath() string { return m.path + ".log" }

// logLayout is the record layout every log transaction is encoded under. It is
// derived from the extension set once and never from the base file, so the log
// stays readable across base-format changes.
var logLayout = sync.OnceValues(func() (mailindex.RecordLayout, error) {
	return mailindex.ComputeRecordLayout(defaultExtensions(0))
})

func entryToLogRecord(e MapEntry) *mailindex.Record {
	return &mailindex.Record{
		UID: e.UID,
		Ext: map[string][]byte{
			extMap:  encodeMapExt(e.FileID, e.Offset, e.Size),
			extRef:  encodeRefExt(e.RefCount),
			extGUID: encodeGUIDExt(e.GUID),
		},
	}
}

func logRecordToEntry(rec mailindex.Record) (MapEntry, error) {
	fileID, offset, size, err := decodeMapExt(rec.Ext[extMap])
	if err != nil {
		return MapEntry{}, fmt.Errorf("decode map ext: %w", err)
	}
	return MapEntry{
		UID:      rec.UID,
		FileID:   fileID,
		Offset:   offset,
		Size:     size,
		RefCount: decodeRefExt(rec.Ext[extRef]),
		GUID:     decodeGUIDExt(rec.Ext[extGUID]),
	}, nil
}

// openLogLocked opens the log for appending, writing its header when new. The
// header carries the base's lineage, so a reader can tell whether the base it
// read already contains these transactions. The base is not touched.
func (m *Map) openLogLocked() (*os.File, error) {
	f, err := os.OpenFile(m.logPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mdboxmap/log: open: %w", err)
	}
	st, _ := f.Stat()
	if st == nil || st.Size() != 0 {
		return f, nil
	}
	// v1 has nowhere in its base to record a lineage, so a log written under it
	// carries the constant its readers have always expected.
	seq := m.lineage
	if m.format == FormatV1 {
		seq = 1
	}
	hdr := mailindex.NewLogHeader(m.indexID, seq, uint32(time.Now().Unix()))
	if err := hdr.Encode(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mdboxmap/log: header: %w", err)
	}
	m.logLineage = seq
	m.logSize = mailindex.LogHeaderSize
	return f, nil
}

// logIdentityLocked reports the sequence number and byte size of the log on
// disk. seq is 0 when there is no readable log.
func (m *Map) logIdentityLocked() (uint32, int64, error) {
	f, err := os.Open(m.logPath())
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("mdboxmap/log: identity: %w", err)
	}
	defer f.Close()
	st, serr := f.Stat()
	if serr != nil {
		return 0, 0, fmt.Errorf("mdboxmap/log: identity stat: %w", serr)
	}
	lh, derr := mailindex.DecodeLogHeader(f)
	if derr != nil {
		return 0, st.Size(), nil
	}
	return lh.FileSeq, st.Size(), nil
}

// appendLogLocked appends entries to the map log as a single TxAppend
// transaction. Caller holds m.mu and the cross-process map lock.
func (m *Map) appendLogLocked(entries []MapEntry) error {
	if len(entries) == 0 {
		return nil
	}
	layout, err := logLayout()
	if err != nil {
		return fmt.Errorf("mdboxmap/log: layout: %w", err)
	}
	recs := make([]*mailindex.Record, len(entries))
	for i, e := range entries {
		recs[i] = entryToLogRecord(e)
	}
	payload, err := mailindex.EncodeTxAppendPayload(layout, recs)
	if err != nil {
		return fmt.Errorf("mdboxmap/log: encode: %w", err)
	}
	return m.writeLogTxLocked(mailindex.TxTypeAppend, payload)
}

// appendRefcountLogLocked logs refcount changes as deltas, not absolutes, which
// would be safe only while every writer holds the lock and has reloaded.
func (m *Map) appendRefcountLogLocked(deltas []mailindex.TxExtAtomicInc) error {
	if len(deltas) == 0 {
		return nil
	}
	if err := m.writeLogTxLocked(mailindex.TxTypeExtAtomicInc, mailindex.EncodeTxExtAtomicIncPayload(deltas)); err != nil {
		return err
	}
	// Same rotation policy the append path uses: the log is bounded, not
	// unbounded.
	if m.shouldRotateLocked() {
		return m.flushLocked()
	}
	return nil
}

// shouldRotateLocked applies the rotation triple; caller holds m.mu and the map
// lock. Age comes from the base's mtime -- the last fold, read the same in every
// process, where a handle-local stamp folds on each session's first write.
func (m *Map) shouldRotateLocked() bool {
	minSize := m.logRotateMinSizeOrDefault()
	if minSize == 0 {
		return false // rotation disabled
	}
	if m.logSize > m.logRotateMaxSizeOrDefault() {
		return true
	}
	if m.logSize < minSize {
		return false
	}
	return m.sinceLastFoldLocked() >= m.logRotateMinAgeOrDefault()
}

// sinceLastFoldLocked reports how long ago the base was last written. An
// unreadable or absent base reads as infinitely old: a map whose base cannot be
// stat'd should fold on size alone rather than never.
func (m *Map) sinceLastFoldLocked() time.Duration {
	fi := m.baseInfo
	if fi == nil {
		var err error
		fi, err = os.Stat(m.path)
		if err != nil {
			return time.Duration(math.MaxInt64)
		}
	}
	return time.Since(fi.ModTime())
}

func (m *Map) writeLogTxLocked(txType mailindex.TxType, payload []byte) error {
	f, err := m.openLogLocked()
	if err != nil {
		return err
	}
	defer f.Close()
	rec, err := encMapLogRec(txType, payload)
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

// commitAppendLocked persists the freshly-appended entries: it writes them to
// the append log (the incremental hot path) and folds the log into the base when
// the rotation triple says so. Caller holds m.mu + the map lock.
func (m *Map) commitAppendLocked(entries []MapEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := m.appendLogLocked(entries); err != nil {
		return err
	}
	if m.shouldRotateLocked() {
		return m.flushLocked()
	}
	return nil
}

// encMapLogRec frames one transaction: an 8-byte header and the payload, padded
// to a 4-byte boundary as the framing requires. A reader strides whole records
// and stops before the pad.
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

// replayFromPersistedLocked replays what the base lacks, by lineage: its own log
// whole, the folded one past its offset, anything else not at all.
func (m *Map) replayFromPersistedLocked() (int64, error) {
	seq, _, err := m.logIdentityLocked()
	if err != nil {
		return 0, err
	}
	if seq == 0 {
		return 0, nil
	}
	if m.onDisk == FormatV1 {
		// No lineage to compare against: v1 replays whole and lives with what
		// that costs (see formatv1.go).
		m.logLineage = seq
		return m.replayLogLocked(mailindex.LogHeaderSize)
	}
	var from int64
	switch seq {
	case m.lineage:
		from = mailindex.LogHeaderSize
	case m.foldedLineage:
		from = m.foldedOffset
		if from < mailindex.LogHeaderSize {
			from = mailindex.LogHeaderSize
		}
	default:
		return 0, errLogIndexMismatch
	}
	m.logLineage = seq
	return m.replayLogLocked(from)
}

// replayLogLocked folds transactions from fromOffset into the record area and
// returns the new applied offset. A torn tail stops replay cleanly without
// consuming the partial record. Caller holds m.mu.
func (m *Map) replayLogLocked(fromOffset int64) (off int64, err error) {
	start := time.Now()
	defer func() { m.observePart("replay", time.Since(start)) }()
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
	if lh.IndexID != m.indexID {
		return fromOffset, errLogIndexMismatch
	}
	pos := int64(mailindex.LogHeaderSize)
	if fromOffset > pos {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return fromOffset, fmt.Errorf("mdboxmap/log: seek: %w", err)
		}
		pos = fromOffset
	}
	layout, err := logLayout()
	if err != nil {
		return fromOffset, fmt.Errorf("mdboxmap/log: layout: %w", err)
	}
	stride := int(layout.RecordSize)
	if stride == 0 {
		return pos, nil
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
			m.applyRefcountDeltasLocked(payload)
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
			if _, dup := m.st.find(rec.UID); dup {
				continue
			}
			e, cerr := logRecordToEntry(rec)
			if cerr != nil {
				break
			}
			m.st.insert(e)
			if e.UID >= m.nextMapUID {
				m.nextMapUID = e.UID + 1
			}
			if e.FileID > m.highestFileID {
				m.highestFileID = e.FileID
			}
		}
	}
	return pos, nil
}

// applyRefcountDeltasLocked folds one increment payload in. An unknown uid is
// skipped, safe only while a record is durable before any delta against it.
func (m *Map) applyRefcountDeltasLocked(payload []byte) {
	for _, d := range mailindex.DecodeTxExtAtomicIncPayload(payload) {
		i, ok := m.st.find(d.UID)
		if !ok {
			continue
		}
		e := m.st.at(i)
		e.RefCount = clampRef(int32(e.RefCount) + d.Diff)
		m.st.setAt(i, e)
	}
}

// clampRef keeps a refcount inside the 16 bits a record carries. The floor is
// what matters: below zero it wraps huge and keeps a dead file alive forever.
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
