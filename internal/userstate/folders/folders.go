// Package folders keeps one durable per-user record of folder identity: for
// each folder name, the UIDVALIDITY it was created with, and the watermark that
// makes a fresh one impossible to repeat.
//
// It exists because a folder's UIDVALIDITY lived in exactly one place -- that
// folder's index -- so losing the index lost the identity, and every client
// resynchronised from scratch. The other implementation keeps the same
// per-folder identity in its mailbox list index, and treats it as a cache: it
// sits with the indexes and dies with them, and its values are refreshed from
// each folder's own index. This record is deliberately stronger in two ways
// (#1611):
//
//   - it lives with the mail, under the control root, so the operator gesture
//     that removes index files does not remove it;
//   - it is the authority for a folder's identity whenever that folder's index
//     cannot answer, rather than a copy that may lag.
//
// The watermark is the piece their design keeps separately, in a durable
// allocator file beside the mail, and the reason is worth restating: without
// one, a fresh UIDVALIDITY comes from the clock, and a folder deleted and
// created again inside one second is handed the same number with different
// contents -- which RFC 3501 §6.3.4 forbids precisely because a client's cached
// UIDs would still look valid.
package folders

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// FileName is the record's name under the control root.
const FileName = "yarilo.folders"

// foldThreshold is the line count past which a read folds the file back to one
// line per surviving folder. Chosen so an ordinary mailbox never folds and a
// mailbox that churns folders does it rarely.
const foldThreshold = 4096

// Store is one user's folder-identity record.
type Store struct {
	path   string
	user   string
	owner  string
	locker locks.Locker
}

// New returns the store for a user. root is the control root -- the same
// directory the subscriptions live in, by the same rule, so this cannot drift
// from where the rest of a user's control state is written.
func New(root, user, owner string, l locks.Locker) *Store {
	return &Store{path: filepath.Join(root, FileName), user: user, owner: owner, locker: l}
}

type state struct {
	uidValidity map[string]uint32
	created     map[string]int64
	watermark   uint32
	lines       int
}

// Allocate returns a UIDVALIDITY that has never been handed out before, and
// raises the watermark past it.
//
// requested is what the caller would have used -- a wall-clock stamp, today --
// and it is honoured when it is above the watermark. That keeps the values
// recognisable as timestamps on a healthy system while making a repeat
// impossible on an unhealthy one: a clock that steps backwards, or two folders
// created inside one second, both get the watermark instead.
func (s *Store) Allocate(requested uint32) (uint32, error) {
	var out uint32
	err := s.withLock(func() error {
		st, err := s.load()
		if err != nil {
			return err
		}
		out = requested
		if out < st.watermark {
			out = st.watermark
		}
		if out == 0 {
			out = uint32(time.Now().Unix())
		}
		return s.append(fmt.Sprintf("n %x", uint64(out)+1))
	})
	return out, err
}

// Record stores a folder's identity. Idempotent: recording the same pair twice
// changes nothing a reader sees.
func (s *Store) Record(folder string, uidValidity uint32, created time.Time) error {
	if folder == "" || uidValidity == 0 {
		return nil
	}
	return s.withLock(func() error {
		st, err := s.load()
		if err != nil {
			return err
		}
		if have, ok := st.uidValidity[folder]; ok && have == uidValidity {
			return nil
		}
		defer func() { s.foldIfLong(st) }()
		line := fmt.Sprintf("+ %x %x %s", uidValidity, created.Unix(), folder)
		if uidValidity >= st.watermark {
			// One append rather than two: the watermark has to pass a value
			// that was not allocated here -- an adopted folder carries theirs.
			if err := s.append(fmt.Sprintf("n %x", uint64(uidValidity)+1)); err != nil {
				return err
			}
		}
		return s.append(line)
	})
}

// UIDValidity returns what this folder was created with, and whether the record
// knows it.
func (s *Store) UIDValidity(folder string) (uint32, bool, error) {
	var v uint32
	var ok bool
	err := s.withLock(func() error {
		st, lerr := s.load()
		if lerr != nil {
			return lerr
		}
		v, ok = st.uidValidity[folder]
		return nil
	})
	return v, ok, err
}

// Remove drops a folder's identity, which a DELETE must do: RFC 3501 §6.3.4
// requires a folder created again under the same name to get a new
// UIDVALIDITY, and an entry surviving the delete would hand back the old one.
func (s *Store) Remove(folder string) error {
	return s.withLock(func() error {
		st, err := s.load()
		if err != nil {
			return err
		}
		if _, ok := st.uidValidity[folder]; !ok {
			return nil
		}
		defer func() { s.foldIfLong(st) }()
		return s.append("- " + folder)
	})
}

// Rename moves a folder's identity, keeping the UIDVALIDITY: that is what
// RENAME means to a client.
func (s *Store) Rename(oldName, newName string) error {
	return s.withLock(func() error {
		st, err := s.load()
		if err != nil {
			return err
		}
		v, ok := st.uidValidity[oldName]
		if !ok {
			return nil
		}
		if err := s.append("- " + oldName); err != nil {
			return err
		}
		return s.append(fmt.Sprintf("+ %x %x %s", v, st.created[oldName], newName))
	})
}

// foldIfLong rewrites the file to one line per surviving folder once the
// journal has grown past the bound. A failure is logged by the caller's next
// read finding the same long file: nothing is lost, the file is only longer
// than it needs to be.
func (s *Store) foldIfLong(st *state) {
	if st.lines <= foldThreshold {
		return
	}
	fresh, err := s.load()
	if err != nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "n %x\n", fresh.watermark)
	for name, uv := range fresh.uidValidity {
		fmt.Fprintf(&b, "+ %x %x %s\n", uv, fresh.created[name], name)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
	}
}

// load replays the file. A line that does not parse is skipped rather than
// fatal: this record must never be the reason a mailbox cannot be opened, and
// the worst a skipped line costs is one folder resynchronising.
func (s *Store) load() (*state, error) {
	st := &state{uidValidity: map[string]uint32{}, created: map[string]int64{}}
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("folders: open %s: %w", s.path, err)
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		st.lines++
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "n "):
			if v, perr := strconv.ParseUint(line[2:], 16, 32); perr == nil && uint32(v) > st.watermark {
				st.watermark = uint32(v)
			}
		case strings.HasPrefix(line, "+ "):
			rest := line[2:]
			uv, rest, ok := cutHex(rest)
			if !ok {
				continue
			}
			cr, name, ok := cutHex(rest)
			if !ok || name == "" {
				continue
			}
			st.uidValidity[name] = uint32(uv)
			st.created[name] = int64(cr)
		case strings.HasPrefix(line, "- "):
			name := line[2:]
			delete(st.uidValidity, name)
			delete(st.created, name)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("folders: read %s: %w", s.path, err)
	}
	return st, nil
}

func cutHex(s string) (uint64, string, bool) {
	head, rest, found := strings.Cut(s, " ")
	if !found {
		return 0, "", false
	}
	v, err := strconv.ParseUint(head, 16, 64)
	if err != nil {
		return 0, "", false
	}
	return v, rest, true
}

// append adds one line durably. The fsync is the point: everything this record
// protects is state that has to outlive the crash which lost an index.
func (s *Store) append(line string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("folders: mkdir: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("folders: open for append: %w", err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("folders: append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("folders: sync: %w", err)
	}
	return nil
}

// withLock serialises against every other writer of this user's record. The
// per-user index lock, which is the outer lock in the documented order, so a
// caller already holding it makes no round trip.
func (s *Store) withLock(fn func() error) error {
	if s.locker == nil {
		return fn()
	}
	key := locks.IndexKey(s.user)
	if s.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, s.locker, key, s.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("folders: lock: %w", err)
	}
	defer func() { _ = s.locker.Unlock(ctx, lk.ID) }()
	return fn()
}
