package director

import (
	"encoding/json"
	"fmt"
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
	backendHost, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		apiError(w, "invalid backend address", http.StatusBadRequest)
		return
	}
	s.overrideMu.Lock()
	s.overrides[user] = addr
	s.overrideMu.Unlock()
	s.userDir.Set(user, addr, false)
	s.originateRingEvent("USER-MOVED", fmt.Sprintf("%s\t%s\t%s", user, backendHost, portStr), nil)
	slog.Info("director API: user moved", "user", user, "backend", addr)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiUserKick(w http.ResponseWriter, r *http.Request) {
	user := s.normalizeUser(r.PathValue("user"))
	// Grace window (#740): an admin kick (typically the tail of a move) waits
	// user_kick_delay before the USER-KICKED push so an in-flight command on
	// the old backend can finish. Scheduled off the request goroutine so the
	// API responds immediately; backend-down/expiry kicks are never delayed.
	if delay := s.opts.userKickDelay(); delay > 0 {
		time.AfterFunc(delay, func() { s.originateRingEvent("USER-KICKED", user, nil) })
		slog.Info("director API: user kick scheduled", "user", user, "delay", delay)
	} else {
		s.originateRingEvent("USER-KICKED", user, nil)
		slog.Info("director API: user kicked", "user", user)
	}
	apiJSON(w, map[string]string{"status": "ok"})
}
