package acl

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// ListFileName is the on-disk per-namespace global index of mailboxes
// that carry an explicit yarilo-acl file. One file per namespace root
// at <home>/<ListFileName>; LIST in shared / public namespaces reads
// this index once instead of stat-ing every mailbox dir.
//
// Format — one entry per (mailbox, identifier) tuple, tab-delimited
// fields, LF terminator:
//
//	<mailbox>\t[-]<identifier>\t<rights>\n
//
// Identifier carries the leading '-' for negatives so the wire and
// disk layers stay symmetric with per-mailbox files. Empty rights are
// permitted ("identifier present but grants nothing"). Comment lines
// (#) and blank lines are skipped on read; writes never produce them.
// Entries are sorted by (mailbox, identifier, negative) so two
// processes rewriting the same set produce byte-identical files.
const ListFileName = "yarilo-acl-list"

// ListEntry is one row of the global index.
type ListEntry struct {
	Mailbox    string
	Identifier mailbox.Identifier
	Rights     mailbox.Rights
	Negative   bool
}

// ListPath returns the on-disk yarilo-acl-list path for this Store's
// namespace root.
func (s *Store) ListPath() string { return filepath.Join(s.home, ListFileName) }

// ListSnapshot returns every entry in the on-disk index. Missing file
// returns (nil, nil) — "no mailbox in this namespace has an explicit
// ACL yet" is the normal initial state.
func (s *Store) ListSnapshot() ([]ListEntry, error) {
	var (
		out []ListEntry
		err error
	)
	werr := s.withListLock(func() error {
		out, err = s.loadListLocked()
		return err
	})
	if werr != nil {
		return nil, werr
	}
	return out, nil
}

// ListLookup returns the names of every mailbox in this namespace
// that grants the lookup ('l') right to the accessing user. Used by
// LIST in shared / public namespaces to short-circuit a per-mailbox
// ACL walk when the index covers everything.
//
// Owner short-circuit is the caller's job (an owner sees everything
// in their personal namespace whether or not the index lists it);
// ListLookup is the strictly non-owner path.
func (s *Store) ListLookup(user string, groups []string) (map[string]struct{}, error) {
	entries, err := s.ListSnapshot()
	if err != nil {
		return nil, err
	}
	// Group entries by mailbox so we can apply the same
	// positive-minus-negative semantics ACL.Effective uses on
	// per-mailbox files.
	byBox := map[string]mailbox.ACL{}
	for _, e := range entries {
		byBox[e.Mailbox] = append(byBox[e.Mailbox], mailbox.Entry{
			Identifier: e.Identifier,
			Rights:     e.Rights,
			Negative:   e.Negative,
		})
	}
	out := map[string]struct{}{}
	for mbox, acl := range byBox {
		if acl.Effective(user, groups, false).Has(mailbox.RightLookup) {
			out[mbox] = struct{}{}
		}
	}
	return out, nil
}

// ListUpdate replaces every entry for folder with the supplied ACL.
// Passing a nil acl is equivalent to ListRemove(folder).
//
// Called automatically by Set / Update / Remove so callers normally
// do not need to invoke this directly. Exposed for admin tooling
// (rebuild) and tests.
func (s *Store) ListUpdate(folder string, acl mailbox.ACL) error {
	return s.withListLock(func() error {
		entries, err := s.loadListLocked()
		if err != nil {
			return err
		}
		entries = filterListEntries(entries, folder)
		for _, e := range acl {
			entries = append(entries, ListEntry{
				Mailbox:    folder,
				Identifier: e.Identifier,
				Rights:     e.Rights,
				Negative:   e.Negative,
			})
		}
		return s.writeListAtomicLocked(entries)
	})
}

// ListRemove drops every entry for folder. Idempotent.
func (s *Store) ListRemove(folder string) error {
	return s.withListLock(func() error {
		entries, err := s.loadListLocked()
		if err != nil {
			return err
		}
		filtered := filterListEntries(entries, folder)
		if len(filtered) == len(entries) {
			return nil
		}
		return s.writeListAtomicLocked(filtered)
	})
}

// ListRename rewrites every entry for oldFolder under newFolder.
// Idempotent on (no entries for oldFolder).
func (s *Store) ListRename(oldFolder, newFolder string) error {
	return s.withListLock(func() error {
		entries, err := s.loadListLocked()
		if err != nil {
			return err
		}
		var changed bool
		for i := range entries {
			if entries[i].Mailbox == oldFolder {
				entries[i].Mailbox = newFolder
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return s.writeListAtomicLocked(entries)
	})
}

// ListRebuild reseeds the index from scratch using a caller-supplied
// resolver that loads each folder's ACL. Used by admin tooling and
// recovery paths when the index drifts from the per-mailbox files
// (e.g. operator edits a yarilo-acl file by hand without going
// through SETACL).
//
// resolveACL is called for each folder under the lock; it MUST NOT
// re-enter the Store (no Set / Get / ListUpdate calls), or it will
// deadlock against the list lock.
func (s *Store) ListRebuild(folders []string, resolveACL func(folder string) (mailbox.ACL, error)) error {
	return s.withListLock(func() error {
		var entries []ListEntry
		for _, folder := range folders {
			acl, err := resolveACL(folder)
			if err != nil {
				return fmt.Errorf("userstate/acl: rebuild %s: %w", folder, err)
			}
			for _, e := range acl {
				entries = append(entries, ListEntry{
					Mailbox:    folder,
					Identifier: e.Identifier,
					Rights:     e.Rights,
					Negative:   e.Negative,
				})
			}
		}
		return s.writeListAtomicLocked(entries)
	})
}

// ---- internal helpers ----

func filterListEntries(in []ListEntry, dropMailbox string) []ListEntry {
	out := in[:0]
	for _, e := range in {
		if e.Mailbox == dropMailbox {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (s *Store) loadListLocked() ([]ListEntry, error) {
	path := s.ListPath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("userstate/acl: list open %s: %w", path, err)
	}
	defer f.Close()
	var out []ListEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimRight(sc.Text(), "\r")
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		fields := strings.Split(raw, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("userstate/acl: list line %d: want 3 tab fields, got %d", line, len(fields))
		}
		negative := false
		idStr := fields[1]
		if strings.HasPrefix(idStr, "-") {
			negative = true
			idStr = idStr[1:]
		}
		id, err := mailbox.ParseIdentifier(idStr)
		if err != nil {
			return nil, fmt.Errorf("userstate/acl: list line %d identifier: %w", line, err)
		}
		rights, err := mailbox.ParseRights(fields[2])
		if err != nil {
			return nil, fmt.Errorf("userstate/acl: list line %d rights: %w", line, err)
		}
		out = append(out, ListEntry{
			Mailbox:    fields[0],
			Identifier: id,
			Rights:     rights,
			Negative:   negative,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("userstate/acl: list read: %w", err)
	}
	return out, nil
}

func (s *Store) writeListAtomicLocked(entries []ListEntry) error {
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return fmt.Errorf("userstate/acl: list mkdir %s: %w", s.home, err)
	}
	// Sorted by (mailbox, identifier-canonical, negative) so two
	// processes produce byte-identical files.
	sorted := make([]ListEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Mailbox != sorted[j].Mailbox {
			return sorted[i].Mailbox < sorted[j].Mailbox
		}
		ii, ij := sorted[i].Identifier.String(), sorted[j].Identifier.String()
		if ii != ij {
			return ii < ij
		}
		return !sorted[i].Negative && sorted[j].Negative
	})

	tmp := s.ListPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("userstate/acl: list tmp %s: %w", tmp, err)
	}
	bw := bufio.NewWriter(f)
	for _, e := range sorted {
		idStr := e.Identifier.String()
		if e.Negative {
			idStr = "-" + idStr
		}
		if _, err := fmt.Fprintf(bw, "%s\t%s\t%s\n", e.Mailbox, idStr, e.Rights.String()); err != nil {
			f.Close()
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("userstate/acl: list write: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/acl: list flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/acl: list close: %w", err)
	}
	if err := os.Rename(tmp, s.ListPath()); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/acl: list rename: %w", err)
	}
	return nil
}

func (s *Store) withListLock(fn func() error) error {
	if s.locker == nil {
		return fn()
	}
	key := locks.ACLListKey(s.home)
	if s.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, s.locker, key, s.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("userstate/acl: list lock %s: %w", key, err)
	}
	defer func() { _ = s.locker.Unlock(ctx, lk.ID) }()
	return fn()
}
