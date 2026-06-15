// Package managesieve implements the RFC 5804 ManageSieve backend.
// It accepts pre-authenticated connections from a ManageSieve login pod
// (which handled SASL auth) and manages per-user Sieve scripts.
package managesieve

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

const crlf = "\r\n"

// readAtom reads a whitespace-delimited command token and returns it uppercased.
func readAtom(r *bufio.Reader) (string, error) {
	skipWS(r)
	var sb strings.Builder
	for {
		b, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			_ = r.UnreadByte()
			break
		}
		sb.WriteByte(b)
	}
	return strings.ToUpper(sb.String()), nil
}

// skipWS discards ASCII space and tab characters.
func skipWS(r *bufio.Reader) {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		if b != ' ' && b != '\t' {
			_ = r.UnreadByte()
			return
		}
	}
}

// skipLine discards bytes through and including the next LF.
func skipLine(r *bufio.Reader) {
	for {
		b, err := r.ReadByte()
		if err != nil || b == '\n' {
			return
		}
	}
}

// skipCRLF reads and discards a CRLF or bare LF terminator.
func skipCRLF(r *bufio.Reader) {
	b, err := r.ReadByte()
	if err != nil {
		return
	}
	if b == '\r' {
		next, err := r.ReadByte()
		if err != nil || next != '\n' {
			if err == nil {
				_ = r.UnreadByte()
			}
		}
		return
	}
	if b != '\n' {
		_ = r.UnreadByte()
	}
}

// readString reads a ManageSieve string argument — either a quoted string
// ("...") or a literal ({N} or {N+}).  For synchronizing literals {N},
// contFn is called (and flushed) before reading the literal body so the
// caller can send the required continuation OK first.
func readString(r *bufio.Reader, contFn func() error) ([]byte, error) {
	skipWS(r)
	b, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	switch b {
	case '"':
		return readQuoted(r)
	case '{':
		return readLiteral(r, contFn)
	default:
		_ = r.UnreadByte()
		return nil, fmt.Errorf("managesieve: expected string (quoted or literal), got %q", rune(b))
	}
}

func readQuoted(r *bufio.Reader) ([]byte, error) {
	var sb strings.Builder
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("managesieve: unterminated quoted string: %w", err)
		}
		switch b {
		case '"':
			return []byte(sb.String()), nil
		case '\\':
			next, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("managesieve: bad escape: %w", err)
			}
			sb.WriteByte(next)
		default:
			sb.WriteByte(b)
		}
	}
}

func readLiteral(r *bufio.Reader, contFn func() error) ([]byte, error) {
	// '{' already consumed; read size digits, optional '+', then '}'.
	var sizeBuf strings.Builder
	sync := true
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("managesieve: literal size read: %w", err)
		}
		if b == '}' {
			break
		}
		if b == '+' {
			sync = false
			continue
		}
		sizeBuf.WriteByte(b)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeBuf.String()), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("managesieve: literal size parse: %w", err)
	}
	skipCRLF(r)
	if sync && contFn != nil {
		if err := contFn(); err != nil {
			return nil, fmt.Errorf("managesieve: continuation: %w", err)
		}
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("managesieve: literal body: %w", err)
	}
	return data, nil
}

// writeOK writes "OK [msg]\r\n".  Empty msg omits the string argument.
func writeOK(w *bufio.Writer, msg string) error {
	if msg == "" {
		_, err := w.WriteString("OK\r\n")
		return err
	}
	_, err := fmt.Fprintf(w, "OK %s\r\n", quoteStr(msg))
	return err
}

// writeNO writes "NO [(CODE)] [msg]\r\n".
func writeNO(w *bufio.Writer, code, msg string) error {
	var sb strings.Builder
	sb.WriteString("NO")
	if code != "" {
		sb.WriteString(" (")
		sb.WriteString(code)
		sb.WriteByte(')')
	}
	if msg != "" {
		sb.WriteByte(' ')
		sb.WriteString(quoteStr(msg))
	}
	sb.WriteString(crlf)
	_, err := w.WriteString(sb.String())
	return err
}

// writeBYE writes "BYE [msg]\r\n".
func writeBYE(w *bufio.Writer, msg string) error {
	if msg == "" {
		_, err := w.WriteString("BYE\r\n")
		return err
	}
	_, err := fmt.Fprintf(w, "BYE %s\r\n", quoteStr(msg))
	return err
}

// writeContinue sends an OK continuation before a synchronizing literal body.
func writeContinue(w *bufio.Writer) error {
	if _, err := w.WriteString("OK\r\n"); err != nil {
		return err
	}
	return w.Flush()
}

// writeCapabilities writes the ManageSieve capability block (no trailing OK).
func writeCapabilities(w *bufio.Writer, exts []string) error {
	type kv struct{ k, v string }
	caps := []kv{
		{"IMPLEMENTATION", "yarilo ManageSieve"},
		{"SIEVE", strings.Join(exts, " ")},
		{"VERSION", "1.0"},
	}
	for _, c := range caps {
		if _, err := fmt.Fprintf(w, "%s %s\r\n", quoteStr(c.k), quoteStr(c.v)); err != nil {
			return err
		}
	}
	return nil
}

// readLastArg reads the final string argument of a command.
// For quoted strings the trailing CRLF (rest of command line) is consumed.
// For literals the CRLF was already consumed by the literal framing — no
// additional skipLine is needed.
func readLastArg(r *bufio.Reader, contFn func() error) ([]byte, error) {
	skipWS(r)
	b, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	switch b {
	case '"':
		data, err := readQuoted(r)
		if err != nil {
			return nil, err
		}
		skipLine(r)
		return data, nil
	case '{':
		return readLiteral(r, contFn)
	default:
		_ = r.UnreadByte()
		return nil, fmt.Errorf("managesieve: expected string (quoted or literal), got %q", rune(b))
	}
}

// writeLiteral writes {N}\r\n<data> without a trailing CRLF.
func writeLiteral(w *bufio.Writer, data []byte) error {
	if _, err := fmt.Fprintf(w, "{%d}\r\n", len(data)); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// quoteStr returns s as a ManageSieve quoted string.
func quoteStr(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		default:
			if !unicode.IsPrint(r) {
				sb.WriteByte('?')
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}
