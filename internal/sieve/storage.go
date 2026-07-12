package sieve

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// DefaultScriptBody is the content written by FsInitUser on first delivery.
const DefaultScriptBody = "keep;\n"

const (
	vacationFileName = ".yarilo.sieve-vacation"
	vacationVersion  = uint32(1)
)

func vacationFilePath(homeDir string) string {
	return filepath.Join(homeDir, vacationFileName)
}

func vacationID(handle, senderAddr string) string {
	return handle + "/" + senderAddr
}

type vacationRecord struct {
	expiresAt uint32
	id        string
}

func readVacationRecords(homeDir string) ([]vacationRecord, error) {
	data, err := os.ReadFile(vacationFilePath(homeDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sieve/vacation: read file: %w", err)
	}
	if len(data) < 4 {
		return nil, nil
	}
	if binary.LittleEndian.Uint32(data[:4]) != vacationVersion {
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
		id := string(data[:idLen])
		data = data[idLen:]
		records = append(records, vacationRecord{expiresAt: exp, id: id})
	}
	return records, nil
}

func writeVacationRecords(homeDir string, records []vacationRecord) error {
	var buf bytes.Buffer
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[:4], vacationVersion)
	buf.Write(hdr[:4])
	for _, r := range records {
		idBytes := []byte(r.id)
		binary.LittleEndian.PutUint32(hdr[0:4], r.expiresAt)
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(idBytes)))
		buf.Write(hdr[:8])
		buf.Write(idBytes)
	}
	tmp := vacationFilePath(homeDir) + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("sieve/vacation: write tmp: %w", err)
	}
	return os.Rename(tmp, vacationFilePath(homeDir))
}

// vacationSent reports whether a vacation reply was already sent for handle+senderAddr.
func vacationSent(_ context.Context, homeDir, handle, senderAddr string) (bool, error) {
	records, err := readVacationRecords(homeDir)
	if err != nil {
		return false, err
	}
	now := uint32(time.Now().Unix())
	id := vacationID(handle, senderAddr)
	for _, r := range records {
		if r.expiresAt > now && r.id == id {
			return true, nil
		}
	}
	return false, nil
}

// markVacationSent records a sent vacation reply and evicts expired entries.
func markVacationSent(ctx context.Context, l locks.Locker, homeDir, handle, senderAddr string, ttlSecs int) error {
	return withSieveLock(ctx, l, "sieve-vacation:"+vacationFilePath(homeDir), func(ctx context.Context) error {
		records, err := readVacationRecords(homeDir)
		if err != nil {
			return err
		}
		now := uint32(time.Now().Unix())
		active := records[:0]
		for _, r := range records {
			if r.expiresAt > now {
				active = append(active, r)
			}
		}
		active = append(active, vacationRecord{
			expiresAt: now + uint32(ttlSecs), //nolint:gosec
			id:        vacationID(handle, senderAddr),
		})
		return writeVacationRecords(homeDir, active)
	})
}
