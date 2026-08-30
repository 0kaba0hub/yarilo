package subs

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
)

// Their file's version-2 header. Version 1 has none, and its lines are whole
// names rather than tab-separated levels.
var subsV2Header = []byte("V\t2\n\n")

// Their escape byte, and what follows it. Not a backslash: a level is escaped
// with 0x01 so that a tab inside a mailbox name cannot be read as the level
// separator (lib/strescape.c).
const subsEscape = 0x01

// ReadForeign returns the folder names another implementation's subscription
// file holds, in the form a client sees them.
//
// Their file and ours share a name and, on many deployments, a directory: ours
// lives in the control root, which is the mail path unless a deployment moves
// it, and theirs lives with the mail. So this is not only the conversion's
// business -- an ordinary read has to tell the two apart, or a store nobody has
// converted yet answers LIST with their version header as a subscribed folder
// (#1583).
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
func ReadForeign(path, sep string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("userstate/subs: read %s: %w", path, err)
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
			return nil, fmt.Errorf("userstate/subs: subscription %q in %s: %w", line, path, derr)
		}
		out = append(out, decoded)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("userstate/subs: read %s: %w", path, err)
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

// looksForeign reports whether the bytes are another implementation's
// subscription file rather than ours.
//
// The version header is the only thing that says so, and it is enough: ours is
// plain names, one per line, and a name cannot begin with it -- the header ends
// in a blank line, which ours never contains. A version-1 file of theirs has no
// header and is indistinguishable from ours by shape; it is also identical in
// meaning, one name per line, except that theirs are in modified UTF-7. That
// difference is left alone here rather than guessed at: a name that merely
// looks like modified UTF-7 may be exactly what a user typed.
func looksForeign(raw []byte) bool {
	return bytes.HasPrefix(raw, subsV2Header)
}
