package director

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

func (s *Server) apiBackendList(w http.ResponseWriter, _ *http.Request) {
	backends := s.ring.Backends()
	sess := s.backendSessionCounts()
	bs := make([]backendDTO, len(backends))
	for i, b := range backends {
		bs[i] = toBackendDTO(backendDTOSource{b.IP, b.Port, b.Tag, b.Up, b.Vhosts, sess[b.IP]})
	}
	apiJSON(w, map[string]any{"backends": bs})
}

func (s *Server) apiBackendAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP     string `json:"ip"`
		Port   int    `json:"port"`
		Tag    string `json:"tag"`
		Vhosts int    `json:"vhosts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.IP == "" || req.Port == 0 {
		apiError(w, "ip and port required", http.StatusBadRequest)
		return
	}
	s.ring.AddBackend(&ring.Backend{
		IP: req.IP, Port: req.Port, Tag: req.Tag, Up: true, Vhosts: req.Vhosts,
		LastUp: time.Now().Unix(),
	})
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tup\t%s", req.IP, req.Tag), nil)
	s.updateMetrics()
	slog.Info("director API: backend added", "ip", req.IP, "port", req.Port, "tag", req.Tag)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiBackendUpdate(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	var req struct {
		Vhosts int `json:"vhosts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, "invalid body", http.StatusBadRequest)
		return
	}
	found := false
	tag := ""
	for _, b := range s.ring.Backends() {
		if b.IP == ip {
			b.Vhosts = req.Vhosts
			s.ring.AddBackend(&b)
			tag = b.Tag
			found = true
			break
		}
	}
	if !found {
		apiError(w, "backend not found", http.StatusNotFound)
		return
	}
	// Replicate the weight change ring-wide (#706): without this replicas keep
	// their old vhosts and disagree on ring layout until the next handshake.
	// The "vhosts" event carries no seq, so it never turns the backend
	// lease-managed (a static backend must stay non-expirable).
	s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tvhosts\t%s\t%d", ip, tag, req.Vhosts), nil)
	s.updateMetrics()
	slog.Info("director API: backend updated", "ip", ip, "vhosts", req.Vhosts)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiBackendRemove(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	tag := s.backendTag(ip)
	s.ring.RemoveBackend(ip)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tdown\t%s", ip, tag), nil)
	s.updateMetrics()
	slog.Info("director API: backend removed", "ip", ip)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiBackendUp(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	tag := s.backendTag(ip)
	if !s.ring.SetUp(ip, true, time.Now().Unix()) {
		apiError(w, "backend not found", http.StatusNotFound)
		return
	}
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tup\t%s", ip, tag), nil)
	s.updateMetrics()
	slog.Info("director API: backend up", "ip", ip)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiBackendDown(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	tag := s.backendTag(ip)
	if !s.ring.SetUp(ip, false, time.Now().Unix()) {
		apiError(w, "backend not found", http.StatusNotFound)
		return
	}
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tflush\t%s", ip, tag), nil)
	s.updateMetrics()
	slog.Info("director API: backend down", "ip", ip)
	apiJSON(w, map[string]string{"status": "ok"})
}

// apiBackendFlush is the operator-forced EVACUATION (#706): kick the backend's
// sessions and clear its pins so users move off NOW. This is deliberately different from the wire BACKEND-FLUSH
// (handleBackendFlush), which DRAINS without kicking. The kick is origin-local
// (matching the other admin ops); the pin-clear replicates ring-wide via the
// originated flush event, so every replica rehashes away too.
// apiBackendFlush evacuates a backend (or "all"). Query params (#849):
//
//	force=true            immediate mass evacuate — kick every session at once
//	                      (the pre-#849 behaviour).
//	(default, no force)   graceful throttled drain — kick users in a self-clocked
//	                      window of max_parallel confirmed-kills, spreading the
//	                      re-login across surviving pods instead of stampeding them.
//	max_parallel=N        graceful window size; defaults to director_service.max_parallel_moves.
func (s *Server) apiBackendFlush(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	force := boolParam(r, "force")
	maxParallel := s.opts.maxParallelMoves()
	if v, ok := intParam(r, "max_parallel"); ok {
		maxParallel = v
	}

	ips := []string{ip}
	if ip == "all" {
		ips = ips[:0]
		for _, b := range s.ring.Backends() {
			ips = append(ips, b.IP)
		}
	} else if s.ring.GetBackend(ip) == nil {
		apiError(w, "backend not found", http.StatusNotFound)
		return
	}

	if force {
		for _, bip := range ips {
			s.ring.SetUp(bip, false, time.Now().Unix())
			s.kickSessionsForBackend(bip)
			n := s.userDir.DeleteByBackend(bip)
			s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tflush\t%s", bip, s.backendTag(bip)), nil)
			slog.Info("director API: backend flushed (force evacuate)", "ip", bip, "pins_cleared", n)
		}
		s.updateMetrics()
		apiJSON(w, map[string]string{"status": "ok", "mode": "force"})
		return
	}

	queued := 0
	for _, bip := range ips {
		queued += s.startEvacuation(bip, maxParallel)
	}
	s.updateMetrics()
	apiJSON(w, map[string]any{"status": "ok", "mode": "graceful", "users_queued": queued, "max_parallel": maxParallel})
}
