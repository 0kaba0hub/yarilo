package dboxconv

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
)

const foreignSubscriptions = "subscriptions"

// Their file's version-2 header. Version 1 has none, and its lines are whole
// names rather than tab-separated levels.
var subsV2Header = []byte("V\t2\n\n")

// Their escape byte, and what follows it. Not a backslash: a level is escaped
// with 0x01 so that a tab inside a mailbox name cannot be read as the level
// separator (lib/strescape.c).
const subsEscape = 0x01

// HasForeignSubscriptions reports whether the store carries their subscription
// file.
func HasForeignSubscriptions(mailRoot string) bool {
	_, err := os.Stat(filepath.Join(mailRoot, foreignSubscriptions))
	return err == nil
}

// ReadForeignSubscriptions returns the folder names their subscription file
// holds, in the form a client sees them.
//
// Two things about that file are not guessable and were read off a real one
// rather than reasoned about:
//
//   - the names are stored in modified UTF-7, not UTF-8, so a Cyrillic folder
//     appears as &BBIERQRWBDQEPQRW- and reaches a client as mojibake if carried
//     across verbatim;
//   - in version 2 the levels of a nested name are separated by a tab, each
//     level escaped separately, so Archive/2026 is "Archive\t2026". A reader
//     splitting on the hierarchy separator finds one name where there are two
//     levels, and subscribes the user to a folder that does not exist.
//
// Version 1 files have no header and one whole name per line; they are read
// too, because a store that old is exactly the kind somebody migrates.
func ReadForeignSubscriptions(mailRoot string, sep string) ([]string, error) {
	path := filepath.Join(mailRoot, foreignSubscriptions)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dboxconv: read %s: %w", path, err)
	}
	v2 := bytes.HasPrefix(raw, subsV2Header)
	if v2 {
		raw = raw[len(subsV2Header):]
	}
	if sep == "" {
		sep = "/"
	}

	var out []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var name string
		if v2 {
			levels := strings.Split(line, "\t")
			for i, l := range levels {
				levels[i] = unescapeLevel(l)
			}
			name = strings.Join(levels, sep)
		} else {
			name = line
		}
		decoded, derr := mboxenc.FromModUTF7(name)
		if derr != nil {
			return nil, fmt.Errorf("dboxconv: subscription %q in %s: %w", line, path, derr)
		}
		out = append(out, decoded)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("dboxconv: read %s: %w", path, err)
	}
	return out, nil
}

// unescapeLevel undoes their per-level escaping.
func unescapeLevel(s string) string {
	if !strings.ContainsRune(s, subsEscape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != subsEscape || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '0':
			b.WriteByte(0x00)
		case '1':
			b.WriteByte(0x01)
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// RemoveForeignSubscriptions unlinks their subscription file. Store-wide state,
// so it goes with their map rather than with any one folder (#1569).
func RemoveForeignSubscriptions(mailRoot string) error {
	if err := os.Remove(filepath.Join(mailRoot, foreignSubscriptions)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("dboxconv: remove %s: %w", foreignSubscriptions, err)
	}
	return nil
}
