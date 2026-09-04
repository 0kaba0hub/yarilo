// Package uidvalidity hands out per-user UIDVALIDITY values that are never
// handed out twice.
//
// A folder used to take its value straight from the clock, so two folders
// created in one second shared a number -- and so did a folder recreated in the
// same second as its own delete. RFC 3501 §6.3.4 forbids that for a reason a
// client feels directly: cached UIDs from the old folder stay valid-looking for
// the new one (#1614).
//
// The scheme is the other implementation's, read off a store it wrote: a
// counter file that only rises, and a zero-length marker whose name carries the
// value it claims. The marker is created with O_EXCL, so two processes racing
// for one number produce one winner and one retry. No history is kept: this
// answers "never again", not "what was it before" -- that is the record's job
// (#1611), and the two compose.
package uidvalidity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

const (
	// FileName holds the current value, 8 lowercase hex digits.
	FileName = "yarilo-uidvalidity"
	// LegacyFileName is the same file under the name another implementation
	// wrote. Renamed rather than reseeded: the value must never go down.
	LegacyFileName = "dovecot-uidvalidity"

	// maxClaimAttempts bounds the retry loop. Each attempt loses only to
	// another allocator taking the same number, so a handful is already far
	// past what contention produces.
	maxClaimAttempts = 64
)

// Allocator is one user's UIDVALIDITY source.
type Allocator struct {
	dir  string
	user string
	// owner is fixed at construction, so a BUSY names the builder, not the
	// holder: the key is user-wide, with no folder to stamp (#1664).
	owner  string
	locker locks.Locker
}

// New returns the allocator for a user. dir is the control root -- where the
// rest of a user's control state lives, by the same rule, so this cannot drift
// from it.
func New(dir, user, owner string, l locks.Locker) *Allocator {
	return &Allocator{dir: dir, user: user, owner: owner, locker: l}
}

// Next returns a value never returned before for this user.
//
// floor is what the caller would have used on its own -- a wall-clock stamp --
// and it is taken when it is above everything issued so far, which keeps the
// numbers readable as timestamps on a healthy system. A clock that repeats or
// steps backwards simply loses to the counter.
func (a *Allocator) Next(floor uint32) (uint32, error) {
	var out uint32
	err := a.withLock(func() error {
		current, err := a.read()
		if err != nil {
			return err
		}
		candidate := floor
		if candidate <= current {
			candidate = current + 1
		}
		if candidate == 0 {
			candidate = uint32(time.Now().Unix())
		}
		for i := 0; i < maxClaimAttempts; i++ {
			ok, cerr := a.claim(candidate)
			if cerr != nil {
				return cerr
			}
			if ok {
				if err := a.write(candidate); err != nil {
					return err
				}
				out = candidate
				return nil
			}
			// Somebody else holds this number. Theirs, then; take the next.
			candidate++
		}
		return fmt.Errorf("uidvalidity: no free value after %d attempts near %d", maxClaimAttempts, candidate)
	})
	return out, err
}

// claim creates the marker for v, and reports whether this call created it.
// O_EXCL is the whole mechanism: the filesystem decides the winner, so two
// processes with no lock between them still cannot both take one number.
func (a *Allocator) claim(v uint32) (bool, error) {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return false, fmt.Errorf("uidvalidity: mkdir: %w", err)
	}
	path := filepath.Join(a.dir, fmt.Sprintf("%s.%08x", FileName, v))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("uidvalidity: claim %d: %w", v, err)
	}
	return true, f.Close()
}

// read returns the highest value issued so far, or zero when nothing has been.
// An unreadable or unparseable file reads as zero rather than failing: the
// marker files are what actually prevent a repeat, and a value below one
// already claimed loses to them on the next line.
func (a *Allocator) read() (uint32, error) {
	path := filepath.Join(a.dir, FileName)
	body, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, fmt.Errorf("uidvalidity: read: %w", err)
		}
		// A store written by the other implementation carries the same counter
		// under its own name. Renamed, never reseeded: reseeding from the clock
		// could hand back a value it had already issued.
		legacy := filepath.Join(a.dir, LegacyFileName)
		if lb, lerr := os.ReadFile(legacy); lerr == nil {
			if rerr := os.Rename(legacy, path); rerr != nil {
				return 0, fmt.Errorf("uidvalidity: adopt their counter: %w", rerr)
			}
			body = lb
		} else {
			return 0, nil
		}
	}
	var v uint32
	if _, err := fmt.Sscanf(strings.TrimSpace(string(body)), "%x", &v); err != nil {
		return 0, nil
	}
	return v, nil
}

func (a *Allocator) write(v uint32) error {
	path := filepath.Join(a.dir, FileName)
	// A temporary name per writer, not per process: two allocators in one
	// process shared "<name>.tmp.<pid>", and whichever renamed first left the
	// others renaming a file that was no longer there.
	tmp, err := os.CreateTemp(a.dir, FileName+".tmp.*")
	if err != nil {
		return fmt.Errorf("uidvalidity: temp: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(fmt.Sprintf("%08x", v)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("uidvalidity: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("uidvalidity: close: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("uidvalidity: rename: %w", err)
	}
	return nil
}

// withLock serialises this process's allocators against each other. The
// cross-process guarantee is the marker's O_EXCL, not this: a lock cannot cover
// a deployment where the locker is not wired.
func (a *Allocator) withLock(fn func() error) error {
	if a.locker == nil {
		return fn()
	}
	key := locks.IndexKey(a.user)
	if a.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "uidvalidity-write"), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, a.locker, key, a.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("uidvalidity: lock: %w", err)
	}
	defer func() { _ = a.locker.Unlock(ctx, lk.ID) }()
	return fn()
}
