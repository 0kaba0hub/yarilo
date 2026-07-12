package sieve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/foxcpp/go-sieve/interp"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// DictDuplicateTracker backs the Sieve duplicate test (RFC 7352) with a dict,
// so the dedup window survives across processes and pods when the dict is
// redis-backed (the memory driver keeps the previous per-process behaviour).
// It replaces interp.MemoryDuplicateTracker, whose state was lost between pods.
type DictDuplicateTracker struct {
	d        dict.Dict
	username string
}

// NewDictDuplicateTracker binds a tracker to one user's dict namespace.
func NewDictDuplicateTracker(d dict.Dict, username string) *DictDuplicateTracker {
	return &DictDuplicateTracker{d: d, username: username}
}

func (t *DictDuplicateTracker) key(handle, id string) string {
	sum := sha256.Sum256([]byte(id))
	return dict.PathPrivate + dict.Escape(t.username) + "/sieve/duplicate/" +
		dict.Escape(handle) + "/" + hex.EncodeToString(sum[:])
}

// IsDuplicate reports whether (handle, id) was already recorded within its TTL,
// recording it (with a `seconds` TTL) when new. On :last it refreshes the TTL of
// an existing entry. The lookup-then-set is not atomic across pods; a rare
// concurrent double-delivery of the same message may both observe "new" — an
// accepted trade-off matching the vacation dedup path.
func (t *DictDuplicateTracker) IsDuplicate(ctx context.Context, handle, id string, seconds uint32, last bool) (bool, error) {
	key := t.key(handle, id)
	_, found, err := t.d.Lookup(ctx, &dict.OpSettings{Username: t.username}, key)
	if err != nil {
		return false, fmt.Errorf("sieve/duplicate: lookup: %w", err)
	}
	if found {
		if last {
			if err := t.mark(ctx, key, seconds); err != nil {
				return true, err
			}
		}
		return true, nil
	}
	if err := t.mark(ctx, key, seconds); err != nil {
		return false, err
	}
	return false, nil
}

func (t *DictDuplicateTracker) mark(ctx context.Context, key string, seconds uint32) error {
	tx, err := t.d.Begin(ctx, &dict.OpSettings{Username: t.username, ExpireSecs: seconds})
	if err != nil {
		return fmt.Errorf("sieve/duplicate: begin: %w", err)
	}
	if err := tx.Set(key, []byte("1")); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/duplicate: set: %w", err)
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("sieve/duplicate: commit: %w", commitErr(res, err))
	}
	return nil
}

var _ interp.DuplicateTracker = (*DictDuplicateTracker)(nil)
