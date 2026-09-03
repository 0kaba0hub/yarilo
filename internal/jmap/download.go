package jmap

import (
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// downloadPrefix is the path the session resource advertises as downloadUrl:
// /jmap/download/{accountId}/{blobId}/{name}
const downloadPrefix = "/jmap/download/"

// handleDownload streams one blob to the client (RFC 8620 §6.2).
//
// Two properties matter more than the streaming itself. The blob is resolved
// against the authenticated user's own mail before anything is opened, so a
// blobId belonging to somebody else is a 404 that never touched their file —
// ownership is a precondition, not a check applied to an open handle. And the
// body is copied straight through, so a large attachment is never held whole in
// this process or in the login proxy in front of it.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, id identity) {
	accountID, blobID, name, ok := parseDownloadPath(r.URL.Path)
	if !ok {
		jmapcore.WriteProblem(w, http.StatusNotFound, "Malformed download URL")
		return
	}
	if accountID != "" && accountID != id.user {
		// Another account's blob is indistinguishable from one that does not
		// exist: answering differently would confirm what the caller guessed.
		slog.Warn("jmap: download for another account", "user", id.user, "account", accountID)
		jmapcore.WriteProblem(w, http.StatusNotFound, "No such blob")
		return
	}
	if s.opts.Storage == nil {
		jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Mail store unavailable")
		return
	}
	h, err := s.opts.Storage.open(id.user, id.sessionID)
	if err != nil {
		slog.Warn("jmap: download store open failed", "user", id.user, "err", err)
		jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Mail store unavailable")
		return
	}
	defer h.close()

	// Ownership first: the lookup walks this user's own folders, so a blob that
	// is not theirs is simply not found and no file is opened at all.
	found, err := s.findMessages(h, map[string]bool{blobID: true})
	if err != nil {
		slog.Warn("jmap: download lookup failed", "user", id.user, "err", err)
		jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Mail store unavailable")
		return
	}
	ref, ok := found[blobID]
	if !ok {
		jmapcore.WriteProblem(w, http.StatusNotFound, "No such blob")
		return
	}

	rc, err := h.box.Fetch(ref.folder, ref.meta.Filename, ref.meta.AltTier)
	if err != nil {
		slog.Warn("jmap: download fetch failed", "user", id.user, "blob", blobID, "err", err)
		jmapcore.WriteProblem(w, http.StatusNotFound, "No such blob")
		return
	}
	defer rc.Close() //nolint:errcheck

	// application/octet-stream and an attachment disposition: a message body is
	// never rendered inline, so a crafted blob cannot execute in the origin
	// that serves the API.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+quoteFilename(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, rc); err != nil {
		// The status line is already sent, so this can only be logged.
		slog.Debug("jmap: download aborted", "user", id.user, "blob", blobID, "err", err)
	}
}

// parseDownloadPath splits /jmap/download/{accountId}/{blobId}/{name}. The name
// is what the client asked the file to be called and is not otherwise used.
func parseDownloadPath(path string) (accountID, blobID, name string, ok bool) {
	rest := strings.TrimPrefix(path, downloadPrefix)
	if rest == path {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	name = "download"
	if len(parts) == 3 && parts[2] != "" {
		name = parts[2]
	}
	return parts[0], parts[1], name, true
}

// quoteFilename renders a Content-Disposition filename safely: a quote or a
// newline in it would let a caller inject a header.
func quoteFilename(name string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('_')
		case r < 0x20 || r == 0x7f:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
