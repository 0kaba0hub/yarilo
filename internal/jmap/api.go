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

// registry is the method set. The data methods of RFC 8621 join it in later
// phases; Core/echo alone already exercises dispatch, ordering and
// back-references.
func (s *Server) registry() jmapcore.Registry {
	return jmapcore.CoreRegistry()
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
	resp := jmapcore.Execute(r.Context(), req, s.registry(), s.opts.Limits)
	slog.Debug("jmap: api", "user", id.user, "session", id.sessionID, "calls", len(req.MethodCalls))
	jmapcore.WriteJSON(w, http.StatusOK, resp)
}
