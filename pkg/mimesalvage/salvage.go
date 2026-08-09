// Package mimesalvage reads a message whose MIME is damaged.
//
// Real mail is not always well formed, and a parser that refuses it leaves the
// message unreadable to everything downstream. This package keeps what is
// structurally sound and drops only what it must: a header line that cannot be
// a header is discarded, and the message is parsed again without it. What
// comes back is an ordinary entity -- parts, content types and encodings
// intact -- rather than a blob of raw bytes, so a caller treats a damaged
// message exactly as it treats a healthy one.
//
// It imports nothing from yarilo, so it can be lifted out into a library of
// its own without changes.
package mimesalvage

import (
	"bufio"
	"io"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
)

// maxHeaderLine bounds one header line. A line longer than this is not a
// header any implementation would have written, and reading it whole would let
// a single message decide how much memory this costs.
const maxHeaderLine = 64 << 10

// Result describes what had to be given up to read the message.
type Result struct {
	// Salvaged is true when the message did not parse as it stood.
	Salvaged bool
	// DroppedHeaderLines counts the header lines that could not be a header.
	// Zero with Salvaged true means the damage was elsewhere -- in the shape
	// of the header block rather than in any one line.
	DroppedHeaderLines int
}

// Read parses r, repairing the header block if it has to.
//
// A message that parses as it stands is returned untouched, so the healthy
// path costs one type check and nothing else. Only when the parser refuses is
// the header block rebuilt from the lines that are well formed.
//
// The error is returned when even that leaves nothing to parse: no header line
// survived and no header/body boundary was found, which is not a damaged
// message but something that was never one.
func Read(r io.Reader) (*message.Entity, Result, error) {
	// One pass, no rewind: the parser is fed a reader that remembers what it
	// consumed, so the salvage below can start from the beginning without the
	// caller having to provide a seekable stream.
	buffered := &replayReader{src: bufio.NewReader(r)}
	e, err := message.Read(buffered)
	if err == nil || message.IsUnknownCharset(err) {
		return e, Result{}, err
	}
	return salvage(buffered.replay())
}

func salvage(r io.Reader) (*message.Entity, Result, error) {
	br := bufio.NewReader(r)
	var (
		hdr      textproto.Header
		res      = Result{Salvaged: true}
		lastKey  string
		sawBlank bool
		kept     int
		// dropped lines are kept aside: a message with no blank line has its
		// body among them, and discarding them would lose the words the
		// message is about.
		droppedText []string
	)
	for {
		line, err := readLine(br)
		if line == "" && err != nil {
			break
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			sawBlank = true
			break
		}
		// A continuation belongs to the field above it, and only if that field
		// survived: otherwise it is the tail of something already dropped.
		if (strings.HasPrefix(trimmed, " ") || strings.HasPrefix(trimmed, "\t")) && lastKey != "" {
			hdr.Set(lastKey, hdr.Get(lastKey)+" "+strings.TrimSpace(trimmed))
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			res.DroppedHeaderLines++
			droppedText = append(droppedText, trimmed)
			lastKey = ""
			continue
		}
		lastKey = key
		hdr.Add(key, strings.TrimSpace(value))
		kept++
		if err != nil {
			break
		}
	}
	if kept == 0 && !sawBlank {
		return nil, res, ErrUnsalvageable
	}
	var body io.Reader = br
	if !sawBlank && len(droppedText) > 0 {
		// No header/body boundary was ever found, so the lines that could not
		// be headers are the body: they are what the message says, and the
		// alternative is a message with headers and nothing to search.
		body = io.MultiReader(strings.NewReader(strings.Join(droppedText, "\r\n")+"\r\n"), br)
	}
	e, err := message.New(message.Header{Header: hdr}, body)
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, res, err
	}
	return e, res, nil
}

// readLine returns one line, bounded. A line past the bound is truncated and
// the rest of it discarded: the caller drops the line anyway, and this way one
// pathological message cannot be read into memory whole.
func readLine(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, err := br.ReadString('\n')
		if b.Len() < maxHeaderLine {
			b.WriteString(chunk)
		}
		if err != nil || strings.HasSuffix(chunk, "\n") {
			return b.String(), err
		}
	}
}
