package backendapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/internal/warden"
)

// registerSessionRoutes registers session-control routes.
func (s *Server) registerSessionRoutes() {
	s.mux.Handle("POST /api/backend/sessions/kick", s.middleware(s.handleSessionsKick))
	s.mux.Handle("GET /api/backend/warden/dump", s.middleware(s.handleWardenDump))
}

// handleWardenDump returns the warden state snapshot: accounting
// counters with drift, penalty entries with remaining TTL.
func (s *Server) handleWardenDump(w http.ResponseWriter, _ *http.Request) {
	if s.opts.WardenAddr == "" {
		apiError(w, "dump: warden_addr not configured on backendapi", http.StatusServiceUnavailable)
		return
	}
	ac, err := warden.Dial(s.opts.WardenAddr, s.opts.WardenTLS, defaultKickDialTimeout)
	if err != nil {
		apiError(w, "dump: warden dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer ac.Close()

	d, err := ac.Dump()
	if err != nil {
		apiError(w, "dump: "+err.Error(), http.StatusBadGateway)
		return
	}
	apiJSON(w, d)
}

type sessionsKickRequest struct {
	// SessionID is the warden-issued id shown by `who`. The kick is
	// broadcast to all pods; only the owner reacts.
	SessionID string `json:"session_id"`
	// User is advisory, recorded for audit only.
	User string `json:"user"`
	// Protocols narrows the broadcast; empty = all channels.
	Protocols []string `json:"protocols,omitempty"`
}

type sessionsKickResponse struct {
	// EmittedTo lists channels the event was published on;
	// failed channels go to Errors.
	EmittedTo []string `json:"emitted_to"`
	Errors    []string `json:"errors,omitempty"`
}

// kickChannels: default kick:<protocol> fan-out set.
var kickChannels = []string{"imap", "pop3", "submission", "lmtp"}

// defaultKickDialTimeout bounds the warden dial so an outage
// cannot stall the HTTP request.
const defaultKickDialTimeout = 5 * time.Second

// handleSessionsKick EMITs the kick payload to each requested
// channel. Fire-and-forget: the response means "emitted", not
// "confirmed kicked".
func (s *Server) handleSessionsKick(w http.ResponseWriter, r *http.Request) {
	var req sessionsKickRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		apiError(w, "session_id required", http.StatusBadRequest)
		return
	}
	if s.opts.WardenAddr == "" {
		apiError(w, "kick: warden_addr not configured on backendapi", http.StatusServiceUnavailable)
		return
	}

	targets := req.Protocols
	if len(targets) == 0 {
		targets = kickChannels
	}

	ac, err := warden.Dial(s.opts.WardenAddr, s.opts.WardenTLS, defaultKickDialTimeout)
	if err != nil {
		apiError(w, "kick: warden dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer ac.Close()

	resp := sessionsKickResponse{}
	for _, proto := range targets {
		channel := "kick:" + proto
		if err := ac.Emit(channel, req.SessionID); err != nil {
			slog.Warn("backendapi/kick: emit failed", "channel", channel, "err", err)
			resp.Errors = append(resp.Errors, channel+": "+err.Error())
			continue
		}
		resp.EmittedTo = append(resp.EmittedTo, channel)
	}

	if len(resp.EmittedTo) == 0 {
		apiError(w, "kick: all channels failed", http.StatusInternalServerError)
		return
	}
	slog.Info("backendapi/kick: emitted",
		"session", req.SessionID,
		"user", req.User,
		"channels", resp.EmittedTo,
		"failed", len(resp.Errors),
	)
	apiJSON(w, resp)
}
