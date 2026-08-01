package backendapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/internal/warden"
)

// registerSessionRoutes wires the session-control surface. The
// only command shipped today is `kick`; future additions go here.
func (s *Server) registerSessionRoutes() {
	s.mux.Handle("POST /api/backend/sessions/kick", s.middleware(s.handleSessionsKick))
	s.mux.Handle("GET /api/backend/warden/dump", s.middleware(s.handleWardenDump))
}

// handleWardenDump opens a short-lived warden connection and returns the
// admin/debug state snapshot: accounting counters with their live tally (drift)
// and penalty entries with remaining TTL. Backend-agnostic — the warden server
// computes it from whichever state backend is configured.
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
	// SessionID is the warden-issued session identifier (the value
	// `who` surfaces in the ID column). The kick event is
	// broadcast across every login + LMTP pod via the warden
	// pub-sub channel; only the owner reacts.
	SessionID string `json:"session_id"`
	// User is purely advisory — recorded for audit, not used to
	// scope the kick. Empty when the operator only knows the id.
	User string `json:"user"`
	// Protocols narrows the broadcast to specific channels (e.g.
	// `["imap"]`). Empty means fan out to every supported channel
	// — the typical case when the operator only knows the id.
	Protocols []string `json:"protocols,omitempty"`
}

type sessionsKickResponse struct {
	// EmittedTo lists the kick:<protocol> channels the event was
	// successfully published on. A channel that fails publish
	// (transport error) is reported in Errors.
	EmittedTo []string `json:"emitted_to"`
	Errors    []string `json:"errors,omitempty"`
}

// kickChannels lists every kick:<protocol> resource emitted on
// when the request did not specify a subset. Add new entries here
// when a future session binary subscribes to its own channel.
var kickChannels = []string{"imap", "pop3", "submission", "lmtp"}

// defaultKickDialTimeout bounds the warden dial so a partial
// outage cannot stall the HTTP request indefinitely.
const defaultKickDialTimeout = 5 * time.Second

// handleSessionsKick opens a short-lived warden connection and
// EMITs the kick payload to every requested channel. The matching
// session pod (login for IMAP/POP3/Submission, LMTP backend for
// LMTP) reacts by closing its conn; pods without a matching id
// silently ignore.
//
// Fire-and-forget from a correctness standpoint: the response
// reports "emitted", not "confirmed kicked". Operators that need
// confirmation re-run `yarctl backend who` and verify the
// session no longer appears.
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
