package jmap

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// apiPath is the endpoint the session resource advertises as apiUrl.
const apiPath = "/jmap/api/"

// declaredCapabilities is what this server implements. It is the set a client's
// "using" is checked against, and it is passed to jmapcore rather than read
// there.
func declaredCapabilities() []string {
	return []string{jmapcore.CapCore, jmapcore.CapMail}
}

// registry is the method set for one request. It is built per request because
// the data methods close over that user's storage handle; JMAP has no session
// to hang them on.
func (s *Server) registry(h *userHandle, accountID string) jmapcore.Registry {
	reg := jmapcore.CoreRegistry()
	if h != nil {
		for name, entry := range s.mailboxRegistry(h, accountID) {
			reg[name] = entry
		}
	}
	return reg
}

// handleAPI runs one batch of method calls (RFC 8620 §3).
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request, id identity) {
	// The login layer caps the body at jmap_max_size_request before proxying,
	// so this is the backend's own floor rather than the primary check: a
	// request reaching it oversized means the hop was bypassed.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.Limits.MaxSizeRequest))
	if err != nil {
		slog.Warn("jmap: request body rejected", "user", id.user, "session", id.sessionID, "err", err)
		(&jmapcore.RequestError{
			Type:   jmapcore.ProblemLimit,
			Status: http.StatusRequestEntityTooLarge,
			Limit:  "maxSizeRequest",
			Detail: "The request body exceeds maxSizeRequest.",
		}).Write(w)
		return
	}
	req, rerr := jmapcore.ParseRequest(body, declaredCapabilities(), s.opts.Limits)
	if rerr != nil {
		slog.Debug("jmap: request rejected", "user", id.user, "session", id.sessionID, "type", rerr.Type)
		rerr.Write(w)
		return
	}
	// The handle opens after the envelope parses, so a malformed request never
	// touches storage, and closes with the request: JMAP has no session to keep
	// it alive and a cache would need an invalidation story nothing can supply.
	var h *userHandle
	if s.opts.Storage != nil {
		h, err = s.opts.Storage.open(id.user)
		if err != nil {
			slog.Warn("jmap: storage open failed", "user", id.user, "session", id.sessionID, "err", err)
			jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Mail store unavailable")
			return
		}
		defer h.close()
	}

	resp := jmapcore.Execute(r.Context(), req, s.registry(h, id.user), s.opts.Limits)
	slog.Debug("jmap: api", "user", id.user, "session", id.sessionID, "calls", len(req.MethodCalls))
	jmapcore.WriteJSON(w, http.StatusOK, resp)
}
