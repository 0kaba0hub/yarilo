package maildir

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// dotlockSuffix names the lock file beside the list. Ours is the lock service,
// but a foreign process watches for this one and nothing else.
const dotlockSuffix = ".lock"

// uidRecord is one list entry. Sizes are carried only when the name does not
// already spell them, which is the rule the reference writes by.
type uidRecord struct {
	uid      uint32
	base     string
	guid     [16]byte
	hasGUID  bool
	psize    uint32
	vsize    uint32
	hasSizes bool
}

// uidList is a folder's list: the header the index adopts, and the records in
// the order they were written.
type uidList struct {
	uidValidity uint32
	nextUID     uint32
	guid        string
	records     []uidRecord
	torn        bool // a line no rule explains: what follows it is unrecoverable
}

// readUIDListFile parses the whole file. A line that parses adds a record; the
// first that does not ends the list and marks it torn.
func readUIDListFile(path string) (*uidList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	l := &uidList{}
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if strings.HasPrefix(line, "3 ") {
				parseUIDListHeader(line, l)
				continue
			}
			l.torn = true
			break
		}
		rec, ok := parseUIDListRecord(line)
		if !ok {
			l.torn = true
			break
		}
		l.records = append(l.records, rec)
	}
	if err := sc.Err(); err != nil {
		// A line past the scanner's limit is torn in the same sense: what
		// follows it cannot be reached.
		l.torn = true
	}
	return l, nil
}

func parseUIDListHeader(line string, l *uidList) {
	for _, fld := range strings.Fields(line)[1:] {
		if len(fld) < 2 {
			continue
		}
		switch fld[0] {
		case 'V':
			if n, err := strconv.ParseUint(fld[1:], 10, 32); err == nil {
				l.uidValidity = uint32(n)
			}
		case 'N':
			if n, err := strconv.ParseUint(fld[1:], 10, 32); err == nil {
				l.nextUID = uint32(n)
			}
		case 'G':
			l.guid = fld[1:]
		}
	}
}

func parseUIDListRecord(line string) (uidRecord, bool) {
	sep := strings.Index(line, " :")
	if sep < 0 {
		return uidRecord{}, false
	}
	rec := uidRecord{base: line[sep+2:]}
	fields := strings.Fields(line[:sep])
	if len(fields) == 0 {
		return uidRecord{}, false
	}
	uid, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return uidRecord{}, false
	}
	rec.uid = uint32(uid)
	for _, fld := range fields[1:] {
		if len(fld) < 2 {
			continue
		}
		switch fld[0] {
		case 'G':
			raw, derr := hex.DecodeString(fld[1:])
			if derr != nil || len(raw) != 16 {
				continue
			}
			copy(rec.guid[:], raw)
			rec.hasGUID = true
		case 'S':
			if n, perr := strconv.ParseUint(fld[1:], 10, 32); perr == nil {
				rec.psize, rec.hasSizes = uint32(n), true
			}
		case 'W':
			if n, perr := strconv.ParseUint(fld[1:], 10, 32); perr == nil {
				rec.vsize, rec.hasSizes = uint32(n), true
			}
		}
	}
	return rec, true
}

// String renders one record: keys before the separator, base name after it.
func (r uidRecord) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", r.uid)
	if r.hasGUID {
		fmt.Fprintf(&b, " G%s", hex.EncodeToString(r.guid[:]))
	}
	if r.hasSizes {
		fmt.Fprintf(&b, " S%d W%d", r.psize, r.vsize)
	}
	fmt.Fprintf(&b, " :%s", r.base)
	return b.String()
}

// nameCarriesSizes reports whether the base already spells both numbers, in
// which case a record repeating them is the reference's "omit the keys" case.
func nameCarriesSizes(base string) bool {
	return strings.Contains(base, ",S=") && strings.Contains(base, ",W=")
}

// measureSizes reads the file for the two numbers a name does not carry: the
// physical size, and the virtual one a lone LF grows under CRLF.
func measureSizes(path string) (psize, vsize uint32, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck
	c := &sizeCounter{}
	if _, err := io.Copy(c, f); err != nil {
		return 0, 0, err
	}
	return c.phys, c.phys + c.lfNoCR, nil
}

// writeUIDList rewrites the whole file: a temp beside it, then rename, holding
// the dotlock a foreign process looks for. The header's next uid is recomputed
// from the records, so no reader is handed a number already in use.
func (u *userMailbox) writeUIDList(folder string, l *uidList) error {
	path := u.uidListPath(folder)
	unlock, err := u.dotlock(path)
	if err != nil {
		return err
	}
	defer unlock()

	next := l.nextUID
	for _, rec := range l.records {
		if rec.uid >= next {
			next = rec.uid + 1
		}
	}
	if next == 0 {
		next = 1
	}
	l.nextUID = next
	if l.uidValidity == 0 {
		l.uidValidity = uint32(time.Now().Unix())
	}
	if l.guid == "" {
		l.guid = randomGUID()
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("maildir/uidlist: create tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	fmt.Fprintf(bw, "3 V%d N%d G%s\n", l.uidValidity, l.nextUID, l.guid) //nolint:errcheck
	for _, rec := range l.records {
		fmt.Fprintln(bw, rec.String()) //nolint:errcheck
	}
	if err := bw.Flush(); err != nil {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("maildir/uidlist: write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("maildir/uidlist: close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("maildir/uidlist: rename: %w", err)
	}
	return nil
}

// dotlock takes the lock file beside the list with O_EXCL and returns its
// release. A stale one older than staleDotlock is removed, not waited on.
func (u *userMailbox) dotlock(path string) (func(), error) {
	lock := path + dotlockSuffix
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()                              //nolint:errcheck
			return func() { os.Remove(lock) }, nil //nolint:errcheck
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("maildir/uidlist: dotlock: %w", err)
		}
		st, serr := os.Stat(lock)
		if serr == nil && time.Since(st.ModTime()) > staleDotlock {
			slog.Warn("maildir: removing a stale uidlist lock",
				"file", lock, "age_s", int(time.Since(st.ModTime()).Seconds()))
			os.Remove(lock) //nolint:errcheck
			continue
		}
		if attempt >= dotlockAttempts {
			return nil, fmt.Errorf("maildir/uidlist: %s held by another process", lock)
		}
		time.Sleep(dotlockRetry)
	}
}

const (
	// staleDotlock is when a lock file stops meaning "someone is writing" and
	// starts meaning "someone died".
	staleDotlock = 2 * time.Minute
	// dotlockAttempts x dotlockRetry bounds the wait for a live writer.
	dotlockAttempts = 100
	dotlockRetry    = 20 * time.Millisecond
)

// listEntry is one uid the caller has just assigned to a name.
type listEntry struct {
	uid      uint32
	filename string
}

// recordUIDsLocked writes a whole batch of assignments in one rewrite: per
// message the list would be rewritten once for every message it already holds.
func (u *userMailbox) recordUIDsLocked(folder string, entries []listEntry) error {
	if err := u.ensureUIDListLocked(folder); err != nil {
		return err
	}
	path := u.uidListPath(folder)
	l, err := readUIDListFile(path)
	if err != nil {
		return fmt.Errorf("maildir/uidlist: read: %w", err)
	}
	if l.torn {
		u.reportTornUIDList(folder, path, l)
	}
	at := make(map[string]int, len(l.records))
	for i, rec := range l.records {
		at[rec.base] = i
	}
	for _, e := range entries {
		base := maildirBase(e.filename)
		rec := uidRecord{uid: e.uid, base: base}
		if !nameCarriesSizes(base) {
			psize, vsize, merr := measureSizes(filepath.Join(u.folderPath(folder), "cur", e.filename))
			if merr == nil {
				rec.psize, rec.vsize, rec.hasSizes = psize, vsize, true
			}
		}
		if i, ok := at[base]; ok {
			l.records[i] = rec
			continue
		}
		at[base] = len(l.records)
		l.records = append(l.records, rec)
	}
	if err := u.writeUIDList(folder, l); err != nil {
		return err
	}
	u.folderCacheFor(folder).invalidateUIDs()
	return nil
}

// ensureUIDListLocked creates a header-only list for a folder that has none, so
// the folder's UIDVALIDITY exists before its first message and the index adopts
// it while the folder is still empty (#1701).
func (u *userMailbox) ensureUIDListLocked(folder string) error {
	if err := u.migrateLegacyUIDList(folder); err != nil {
		return err
	}
	path := u.uidListPath(folder)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("maildir/uidlist: stat: %w", err)
	}
	if err := os.MkdirAll(u.controlFolderPath(folder), 0o700); err != nil {
		return fmt.Errorf("maildir/uidlist: mkdir control: %w", err)
	}
	return u.writeUIDList(folder, &uidList{})
}
