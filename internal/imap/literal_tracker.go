package imap

// literalTracker follows an IMAP command stream closely enough to tell a
// command line from literal payload. Both connection wrappers in the accept
// chain scan lines -- one to enforce the line-length limit, one to intercept
// ID -- and payload is not lines: a message body may contain anything,
// including bytes that read like a command.
//
// It exists as one type because the two wrappers disagreeing about where a
// literal starts is exactly the defect it was written for: the limit knew
// about literals and the ID interceptor did not, so a body line whose second
// token was "ID" got answered as a command and removed from the stream. The
// literal then made up the missing octets from the command that followed,
// which vanished without a trace (#1370).
type literalTracker struct {
	remaining int64
}

// inLiteral reports whether the next bytes are payload rather than a command.
func (t *literalTracker) inLiteral() bool { return t.remaining > 0 }

// cap limits a read to what is left of the literal, so the byte after it is
// read as the start of a line again. It counts nothing: only bytes that were
// actually passed through are counted, by consumed.
func (t *literalTracker) cap(n int) int {
	if int64(n) > t.remaining {
		n = int(t.remaining)
	}
	return n
}

// consumed counts payload bytes that were passed through.
func (t *literalTracker) consumed(n int) {
	t.remaining -= int64(n)
	if t.remaining < 0 {
		t.remaining = 0
	}
}

// observeLine notes a literal declaration at the end of a command line.
func (t *literalTracker) observeLine(line string) {
	if n := literalSize(line); n > 0 {
		t.remaining = n
	}
}
