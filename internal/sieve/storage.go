package sieve

import (
	"context"
	"fmt"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// DefaultScriptName is the reserved name of the default Sieve entry-point script.
const DefaultScriptName = "yarilo"

// DefaultScriptBody is the content written by FsInitUser on first delivery.
const DefaultScriptBody = "keep;\n"

func opSettings(username, homeDir string) *dict.OpSettings {
	return &dict.OpSettings{Username: username, HomeDir: homeDir}
}

// vacationSent reports whether a vacation reply was already sent to senderAddr
// for the given handle within its dedup interval. Returns false on any lookup
// error so that sending is attempted rather than silently dropped.
func vacationSent(ctx context.Context, d dict.Dict, username, homeDir, senderAddr, handle string) (bool, error) {
	if d == nil {
		return false, nil
	}
	ops := opSettings(username, homeDir)
	key := vacationKey(handle, senderAddr)
	vals, found, err := d.Lookup(ctx, ops, key)
	if err != nil || !found || len(vals) == 0 {
		return false, err
	}
	var ts int64
	if _, err := fmt.Sscanf(string(vals[0]), "%d", &ts); err != nil {
		return false, nil
	}
	_ = ts
	return true, nil
}

// markVacationSent records that a vacation reply was sent to senderAddr for handle.
func markVacationSent(ctx context.Context, d dict.Dict, username, homeDir, senderAddr, handle string, intervalSecs int) error {
	if d == nil {
		return nil
	}
	ops := opSettings(username, homeDir)
	ops.ExpireSecs = uint32(intervalSecs) //nolint:gosec
	key := vacationKey(handle, senderAddr)
	tx, err := d.Begin(ctx, ops)
	if err != nil {
		return fmt.Errorf("sieve/storage: begin vacation mark: %w", err)
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())
	if err := tx.Set(key, []byte(ts)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/storage: set vacation mark: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("sieve/storage: commit vacation mark: %w", err)
	}
	return nil
}

func vacationKey(handle, senderAddr string) string {
	return dict.PathPrivate + "sieve/vacation/" + dict.Escape(handle) + "/" + dict.Escape(senderAddr)
}
