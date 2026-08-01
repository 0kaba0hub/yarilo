package locks

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Wire protocol — TAB-delimited, LF-terminated. Same bytes for embedded
// (Unix socket) and remote (mTLS TCP). See docs/DEPLOYMENT.md §Wire protocol.
//
//	> VERSION\t1\n                                       — handshake
//	< VERSION\t1\tOK\n
//
//	> LOCK\t<resource>\t<owner>\t<ttl_ms>\n
//	< OK\t<lock_id>\n                                    — acquired
//	< BUSY\t<current_owner>\n                            — held by someone else
//
//	> LOCK-SHARED\t<resource>\t<owner>\t<ttl_ms>\n       — multiple concurrent holders allowed;
//	< OK\t<lock_id>\n                                      only blocks against a LOCK (exclusive) holder
//	< BUSY\t<current_owner>\n                            — an exclusive lock is currently held
//
//	LOCK-SHARED stays at protocolVersion "1": older servers reply
//	ERROR\tunknown_command. UNLOCK/RENEW work unchanged for shared lock
//	IDs; the server tracks each lock ID's kind internally.
//
//	> UNLOCK\t<lock_id>\n
//	< OK\n | NOT_FOUND\n
//
//	> RENEW\t<lock_id>\t<new_ttl>\n
//	< OK\n | EXPIRED\n
//
//	> EMIT\t<resource>\t<event_type>\t<payload>\n
//	< OK\n
//
//	> SUBSCRIBE\t<resource>\n
//	< OK\n                                               — server then streams EVENT lines
//	< EVENT\t<resource>\t<event_type>\t<payload>\n       — async, until SUBSCRIBE conn closes
//
//	> COUNTER-INC\t<key>\t<delta>\n
//	< OK\t<new_value>\n                                  — atomic increment, returns the post-increment value
//
// COUNTER-INC stays at protocolVersion "1": older servers reply
// ERROR\tunknown_command, which clients translate into an "unsupported
// wire op" error rather than a protocol break.
const protocolVersion = "1"

// Commands sent by the client.
const (
	cmdVersion    = "VERSION"
	cmdLock       = "LOCK"
	cmdLockShared = "LOCK-SHARED"
	cmdUnlock     = "UNLOCK"
	cmdRenew      = "RENEW"
	cmdEmit       = "EMIT"
	cmdSubscribe  = "SUBSCRIBE"
	cmdCounterInc = "COUNTER-INC"
)

// Responses sent by the server.
const (
	respOK       = "OK"
	respBusy     = "BUSY"
	respNotFound = "NOT_FOUND"
	respExpired  = "EXPIRED"
	respError    = "ERROR"
	respEvent    = "EVENT"
)

// maxLineLen guards against unbounded reads; owners and resource keys are short.
const maxLineLen = 8192

// reader is a line-framed protocol reader.
type reader struct{ br *bufio.Reader }

func newReader(r io.Reader) *reader { return &reader{br: bufio.NewReaderSize(r, 4096)} }

// readFields reads one LF-terminated line and splits on TAB.
// Returns ErrProtocol for over-long lines or partial reads.
func (r *reader) readFields() ([]string, error) {
	line, err := r.br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) > maxLineLen {
		return nil, fmt.Errorf("locks/proto: line too long (%d > %d): %w", len(line), maxLineLen, ErrProtocol)
	}
	line = strings.TrimRight(line, "\n")
	if line == "" {
		return nil, fmt.Errorf("locks/proto: empty line: %w", ErrProtocol)
	}
	return strings.Split(line, "\t"), nil
}

// writeFields writes fields joined by TAB and terminated with LF.
func writeFields(w io.Writer, fields ...string) error {
	for _, f := range fields {
		if strings.ContainsAny(f, "\t\n") {
			return fmt.Errorf("locks/proto: field contains TAB/LF: %w", ErrProtocol)
		}
	}
	line := strings.Join(fields, "\t") + "\n"
	_, err := io.WriteString(w, line)
	return err
}

// formatTTL encodes a duration as milliseconds for the wire.
// Negative or zero durations are rejected — callers must specify a positive TTL.
func formatTTL(ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("locks/proto: ttl must be positive, got %v", ttl)
	}
	return strconv.FormatInt(ttl.Milliseconds(), 10), nil
}

// parseTTL decodes a TTL string from the wire.
func parseTTL(s string) (time.Duration, error) {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("locks/proto: parse ttl %q: %w", s, err)
	}
	if ms <= 0 {
		return 0, fmt.Errorf("locks/proto: non-positive ttl %d", ms)
	}
	return time.Duration(ms) * time.Millisecond, nil
}
