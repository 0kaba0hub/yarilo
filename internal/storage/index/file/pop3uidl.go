package file

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GetPOP3UIDLs reads the per-folder pop3.uidl sidecar mapping
// IMAP UID → POP3 UIDL string. Missing file returns an empty
// map (not an error). Format is TSV: "<uid>\t<uidl>\n".
//
// Sidecar lives next to yarilo.index — yarilo-specific
// addition; not in the canonical wire format. Removed when
// POP3 UIDLs migrate into a proper mail-index extension in a
// later phase.
func (u *userIndex) GetPOP3UIDLs(folderID uint64) (map[uint32]string, error) {
	var out map[uint32]string
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		path := filepath.Join(fs.indexDir, "pop3.uidl")
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			out = map[uint32]string{}
			return nil
		}
		if err != nil {
			return fmt.Errorf("pop3.uidl open: %w", err)
		}
		defer f.Close()
		out = map[uint32]string{}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			tab := strings.IndexByte(line, '\t')
			if tab < 0 {
				continue
			}
			uid64, err := strconv.ParseUint(line[:tab], 10, 32)
			if err != nil {
				continue
			}
			out[uint32(uid64)] = line[tab+1:]
		}
		return sc.Err()
	})
	return out, err
}

// SavePOP3UIDLs writes uidls to the per-folder pop3.uidl
// sidecar atomically (.tmp + rename). Caller's folder lock
// covers concurrent writers; the rename publishes the new
// state to readers.
func (u *userIndex) SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		path := filepath.Join(fs.indexDir, "pop3.uidl")
		tmp := path + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("pop3.uidl write: %w", err)
		}
		bw := bufio.NewWriter(f)
		for uid, uidl := range uidls {
			fmt.Fprintf(bw, "%d\t%s\n", uid, uidl) //nolint:errcheck
		}
		if err := bw.Flush(); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("pop3.uidl flush: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("pop3.uidl close: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("pop3.uidl rename: %w", err)
		}
		return nil
	})
}
