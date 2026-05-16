package director

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
)

func (s *Server) apiUserMove(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
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
	s.broadcast(fmt.Sprintf("USER-MOVED\t%s\t%s\t%s", user, backendHost, portStr), nil)
	slog.Info("director API: user moved", "user", user, "backend", addr)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiUserKick(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	s.broadcast(fmt.Sprintf("USER-KICKED\t%s", user), nil)
	slog.Info("director API: user kicked", "user", user)
	apiJSON(w, map[string]string{"status": "ok"})
}
