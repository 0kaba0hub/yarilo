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
func (s *Server) registry(lazy *lazyStore, accountID string) jmapcore.Registry {
	reg := jmapcore.CoreRegistry()
	if lazy.storage != nil {
		for name, entry := range s.mailboxRegistry(lazy, accountID) {
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
	// The store opens lazily, on the first method that actually needs it, and
	// closes with the request. Opening up front would fail a whole batch on a
	// dependency the batch may not use — and Core/echo, which exists to
	// diagnose exactly that, would be the first casualty.
	lazy := &lazyStore{storage: s.opts.Storage, user: id.user, sessionID: id.sessionID}
	defer lazy.close()

	resp := jmapcore.Execute(r.Context(), req, s.registry(lazy, id.user), s.opts.Limits)
	slog.Debug("jmap: api", "user", id.user, "session", id.sessionID, "calls", len(req.MethodCalls))
	jmapcore.WriteJSON(w, http.StatusOK, resp)
}
