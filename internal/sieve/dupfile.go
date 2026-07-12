package sieve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/foxcpp/go-sieve/interp"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// DefaultDuplicateFileName is used when SieveConfig.DuplicateFile is unset.
const DefaultDuplicateFileName = ".yarilo.sieve-duplicate"

const duplicateVersion = uint32(1)

// FileDuplicateTracker backs the Sieve duplicate test (RFC 7352) with a per-user
// file in the home directory. On shared storage the file is visible to every
// pod, and the whole check-and-record runs under the per-home Sieve lock, so the
// operation is atomic across processes and pods. Records use the same binary
// layout as the vacation dedup file.
type FileDuplicateTracker struct {
	homeDir  string
	fileName string
	locker   locks.Locker
}

// NewFileDuplicateTracker binds a tracker to a user's home directory. An empty
// fileName uses DefaultDuplicateFileName.
func NewFileDuplicateTracker(homeDir, fileName string, locker locks.Locker) *FileDuplicateTracker {
	if fileName == "" {
		fileName = DefaultDuplicateFileName
	}
	return &FileDuplicateTracker{homeDir: homeDir, fileName: fileName, locker: locker}
}

func (t *FileDuplicateTracker) path() string {
	return filepath.Join(t.homeDir, t.fileName)
}

// withLock runs fn while holding a lock scoped to this user's duplicate file
// only — independent of the sieve-script / vacation lock, so a duplicate check
// does not serialise against unrelated writes in the same home. Nil locker
// (unit tests) runs fn directly.
func (t *FileDuplicateTracker) withLock(ctx context.Context, fn func(context.Context) error) error {
	if t.locker == nil {
		return fn(ctx)
	}
	return locks.WithLock(ctx, t.locker, "sieve-duplicate:"+t.path(), lockOwner(), sieveLockTTL, sieveLockRenew, fn)
}

// duplicateID is the record key: the handle plus a hash of the tracking id, so
// arbitrary Message-IDs / header values stay bounded on disk.
func duplicateID(handle, id string) string {
	sum := sha256.Sum256([]byte(id))
	return handle + "/" + hex.EncodeToString(sum[:])
}

func (t *FileDuplicateTracker) readRecords() ([]vacationRecord, error) {
	data, err := os.ReadFile(t.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sieve/duplicate: read file: %w", err)
	}
	if len(data) < 4 || binary.LittleEndian.Uint32(data[:4]) != duplicateVersion {
		return nil, nil
	}
	data = data[4:]
	var records []vacationRecord
	for len(data) >= 8 {
		exp := binary.LittleEndian.Uint32(data[0:4])
		idLen := binary.LittleEndian.Uint32(data[4:8])
		data = data[8:]
		if uint32(len(data)) < idLen {
			break
		}
		records = append(records, vacationRecord{expiresAt: exp, id: string(data[:idLen])})
		data = data[idLen:]
	}
	return records, nil
}

func (t *FileDuplicateTracker) writeRecords(records []vacationRecord) error {
	var buf bytes.Buffer
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[:4], duplicateVersion)
	buf.Write(hdr[:4])
	for _, r := range records {
		idBytes := []byte(r.id)
		binary.LittleEndian.PutUint32(hdr[0:4], r.expiresAt)
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(idBytes)))
		buf.Write(hdr[:8])
		buf.Write(idBytes)
	}
	tmp := t.path() + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("sieve/duplicate: write tmp: %w", err)
	}
	return os.Rename(tmp, t.path())
}

// IsDuplicate atomically checks and records (handle, id): under the per-home
// Sieve lock it purges expired entries, and if the key is present it is a
// duplicate (refreshing the TTL on :last), otherwise it is recorded with a
// `seconds` TTL and reported new.
func (t *FileDuplicateTracker) IsDuplicate(ctx context.Context, handle, id string, seconds uint32, last bool) (bool, error) {
	key := duplicateID(handle, id)
	var dup bool
	err := t.withLock(ctx, func(_ context.Context) error {
		records, err := t.readRecords()
		if err != nil {
			return err
		}
		now := uint32(time.Now().Unix())
		expiry := now + seconds
		active := records[:0]
		found := false
		for _, r := range records {
			if r.expiresAt <= now {
				continue // drop expired
			}
			if r.id == key {
				found = true
				if last {
					r.expiresAt = expiry
				}
			}
			active = append(active, r)
		}
		if found {
			dup = true
			if !last {
				return nil // no write needed
			}
			return t.writeRecords(active)
		}
		active = append(active, vacationRecord{expiresAt: expiry, id: key})
		return t.writeRecords(active)
	})
	return dup, err
}

var _ interp.DuplicateTracker = (*FileDuplicateTracker)(nil)
