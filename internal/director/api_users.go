package director

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) apiUserMove(w http.ResponseWriter, r *http.Request) {
	user := s.normalizeUser(r.PathValue("user"))
	var req struct {
		Backend string `json:"backend"` // "ip:port"
		IP      string `json:"ip"`
		Port    int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, "invalid body", http.StatusBadRequest)
		return
	}
	var addr string
	switch {
	case req.Backend != "":
		addr = req.Backend
	case req.IP != "" && req.Port > 0:
		addr = net.JoinHostPort(req.IP, strconv.Itoa(req.Port))
	default:
		apiError(w, "backend or ip+port required", http.StatusBadRequest)
		return
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		apiError(w, "invalid backend address", http.StatusBadRequest)
		return
	}
	// Move = TTL'd userDir pin + kick old sessions, replicated ring-wide (#708).
	// No permanent overrides map anymore.
	s.moveUser(user, addr, nil)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiUserKick(w http.ResponseWriter, r *http.Request) {
	user := s.normalizeUser(r.PathValue("user"))
	// Mark the user killing (and replicate it) BEFORE the kick, so the ring-wide
	// LOOKUP hold is in force for the whole drain — otherwise the operator kick
	// (yarctl director users kick → this handler) engages none of the #847
	// confirmed-kick protection and leaves the split-writer window open (#858).
	// Mirrors the wire handleUserKick path.
	s.startKilling(HashUsername(user, s.hf))
	// Grace window (#740): an admin kick (typically the tail of a move) waits
	// user_kick_delay before the USER-KICKED push so an in-flight command on
	// the old backend can finish. Scheduled off the request goroutine so the
	// API responds immediately; backend-down/expiry kicks are never delayed.
	// Clearing the sticky pin is the point of a kick (#706): otherwise the
	// kicked user's next connection re-resolves to the SAME still-Up backend and
	// the kick is a routing no-op. Deleted alongside the USER-KICKED push (which
	// replicas apply too), so origin and replicas drop the pin together.
	if delay := s.opts.userKickDelay(); delay > 0 {
		time.AfterFunc(delay, func() {
			s.userDir.Delete(user)
			s.originateRingEvent("USER-KICKED", user, nil)
		})
		slog.Info("director API: user kick scheduled", "user", user, "delay", delay)
	} else {
		s.userDir.Delete(user)
		s.originateRingEvent("USER-KICKED", user, nil)
		slog.Info("director API: user kicked", "user", user)
	}
	apiJSON(w, map[string]string{"status": "ok"})
}
