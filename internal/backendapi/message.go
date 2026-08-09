package backendapi

import (
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/emersion/go-message"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// registerMessageRoutes registers the message-level read. It is the only route
// that returns the content of somebody's mail rather than facts about it, so
// every call says so in the log.
func (s *Server) registerMessageRoutes() {
	s.mux.Handle("POST /api/backend/message/get", s.middleware(s.handleMessageGet))
}

// messageGetRequest names one message and how much of it to return.
//
// UID and GUID are alternatives, never both: a request carrying two ways to
// name a message is a caller that does not know which one it means, and
// guessing on its behalf is how the wrong message gets read.
type messageGetRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
	UID       uint32 `json:"uid"`
	GUID      string `json:"guid"`
	// Mode is "raw" for the message byte for byte, or "mime" for its
	// structure: every header line as written, and each part's headers, with
	// the part bodies left out.
	Mode string `json:"mode"`
}

const (
	modeRaw  = "raw"
	modeMIME = "mime"
)

func (s *Server) handleMessageGet(w http.ResponseWriter, r *http.Request) {
	var req messageGetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.UID == 0) == (req.GUID == "") {
		apiError(w, "name the message by exactly one of uid or guid", http.StatusBadRequest)
		return
	}
	switch req.Mode {
	case modeRaw, modeMIME:
	default:
		apiError(w, `mode must be "raw" or "mime"`, http.StatusBadRequest)
		return
	}

	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()

	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The name enters here from a log line or a human, so it is normalised at
	// this boundary like every other: a decomposed name would address a
	// different tree than the one that holds the message (#1113).
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)
	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		apiError(w, "open folder: "+err.Error(), http.StatusBadRequest)
		return
	}
	metas, err := bundle.idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		apiError(w, "read folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	meta := findMessage(metas, req.UID, req.GUID)
	if meta == nil {
		apiError(w, "no such message in this folder", http.StatusNotFound)
		return
	}

	// Nothing here writes: no \Seen, no modseq, no index update. A diagnostic
	// that changes what it is diagnosing answers a different question than the
	// one that was asked.
	rc, err := bundle.box.Fetch(req.Folder, meta.Filename, meta.AltTier)
	if err != nil {
		apiError(w, "fetch message: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close() //nolint:errcheck

	// The outline is our own rendering, not a message: calling it rfc822 would
	// misdescribe it to anything that reads this API other than yarctl.
	if req.Mode == modeMIME {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "message/rfc822")
	}
	// A second read of the same message, for the case where the structure
	// cannot be parsed: the first reader is spent by then, and what is left in
	// it starts in the middle of a header.
	reopen := func() (io.ReadCloser, error) {
		return bundle.box.Fetch(req.Folder, meta.Filename, meta.AltTier)
	}
	written, werr := writeMessage(w, rc, req.Mode, reopen)

	// The only route that hands over the content of somebody's mail, so it
	// leaves a trace: who was read, which message, in what form, how much.
	slog.Warn("backendapi: message content read",
		"user", req.User, "folder", req.Folder, "uid", meta.UID,
		"guid", mailbox.FormatObjectID(meta.GUID), "mode", req.Mode,
		"bytes", written, "err", werr)
}

// findMessage resolves the one message the request names. A GUID is matched
// inside the named folder only: it identifies a message within a mailbox, and
// searching wider would answer with a message from somewhere the caller did
// not ask about.
func findMessage(metas []*mailbox.MessageMeta, uid uint32, guid string) *mailbox.MessageMeta {
	for _, m := range metas {
		if uid != 0 && m.UID == uid {
			return m
		}
		if guid != "" && strings.EqualFold(hex.EncodeToString(m.GUID[:]), strings.TrimPrefix(guid, "G")) {
			return m
		}
	}
	return nil
}

// writeMessage streams the message in the requested form. Raw is the bytes as
// they are on disk; mime keeps every header line as written and replaces each
// part's body with its size, which is what makes the structure readable
// without carrying megabytes of base64 across the wire.
func writeMessage(w io.Writer, rc io.Reader, mode string, reopen func() (io.ReadCloser, error)) (int64, error) {
	if mode == modeRaw {
		return io.Copy(w, rc)
	}
	return writeMIMEOutline(w, rc, reopen)
}

func writeMIMEOutline(w io.Writer, rc io.Reader, reopen func() (io.ReadCloser, error)) (int64, error) {
	e, err := message.Read(rc)
	if err != nil && e == nil {
		// A structure that will not parse is the case this command exists for,
		// so the message is handed over whole -- read again from the start,
		// because the parser has already consumed part of the first reader and
		// what remains begins in the middle of a header. Handing that over
		// would show an operator damage that is not there.
		src, ferr := reopen()
		if ferr != nil {
			return 0, ferr
		}
		defer src.Close() //nolint:errcheck
		cw := &countingWriter{w: w}
		cw.printf("<... MIME structure unreadable: %v; the raw message follows ...>\r\n", err)
		// The copy goes through cw, so cw.n already holds the marker and the
		// message. Adding io.Copy's own count on top would report twice what
		// was handed over -- in the line an audit reads for exactly that
		// number.
		_, cerr := io.Copy(cw, src)
		return cw.n, cerr
	}
	cw := &countingWriter{w: w}
	writeEntityOutline(cw, e, 0)
	return cw.n, cw.err
}

func writeEntityOutline(w *countingWriter, e *message.Entity, depth int) {
	for f := e.Header.Fields(); f.Next(); {
		raw, rerr := f.Raw()
		if rerr != nil {
			w.printf("%s: %s\r\n", f.Key(), f.Value())
			continue
		}
		w.write(raw)
	}
	w.printf("\r\n")

	if mr := e.MultipartReader(); mr != nil {
		for i := 0; ; i++ {
			part, perr := mr.NextPart()
			if perr != nil {
				return
			}
			w.printf("--- part %d (depth %d) ---\r\n", i+1, depth+1)
			writeEntityOutline(w, part, depth+1)
		}
	}
	n, _ := io.Copy(io.Discard, e.Body)
	w.printf("<... %d bytes of body elided; use mode=raw for the message itself ...>\r\n", n)
}

type countingWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countingWriter) write(p []byte) {
	if c.err != nil {
		return
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.err = err
}

func (c *countingWriter) printf(format string, args ...any) {
	c.write([]byte(fmt.Sprintf(format, args...)))
}

// Write makes countingWriter an io.Writer, so io.Copy can stream through it
// and its byte count stays the one the audit line reports.
func (c *countingWriter) Write(p []byte) (int, error) {
	c.write(p)
	if c.err != nil {
		return 0, c.err
	}
	return len(p), nil
}
