package director

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

func (s *Server) apiBackendList(w http.ResponseWriter, _ *http.Request) {
	backends := s.ring.Backends()
	bs := make([]backendDTO, len(backends))
	for i, b := range backends {
		bs[i] = toBackendDTO(backendDTOSource{b.IP, b.Port, b.Tag, b.Up, b.Vhosts})
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
		LastUp: time.Now().Unix(), Source: "admin", // #776: never pruned by DNS re-resolution
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
	for _, b := range s.ring.Backends() {
		if b.IP == ip {
			b.Vhosts = req.Vhosts
			s.ring.AddBackend(&b)
			found = true
			break
		}
	}
	if !found {
		apiError(w, "backend not found", http.StatusNotFound)
		return
	}
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

func (s *Server) apiBackendFlush(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if ip == "all" {
		for _, b := range s.ring.Backends() {
			s.ring.SetUp(b.IP, false, time.Now().Unix())
			s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tflush\t%s", b.IP, b.Tag), nil)
		}
		s.updateMetrics()
		slog.Info("director API: all backends flushed")
		apiJSON(w, map[string]string{"status": "ok"})
		return
	}
	tag := s.backendTag(ip)
	if !s.ring.SetUp(ip, false, time.Now().Unix()) {
		apiError(w, "backend not found", http.StatusNotFound)
		return
	}
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tflush\t%s", ip, tag), nil)
	s.updateMetrics()
	slog.Info("director API: backend flushed", "ip", ip)
	apiJSON(w, map[string]string{"status": "ok"})
}
