package director

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

func (s *Server) apiPeerList(w http.ResponseWriter, _ *http.Request) {
	apiJSON(w, map[string]any{"peers": s.ListPeers()})
}

func (s *Server) apiPeerAdd(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Addr == "" {
		apiError(w, "addr required", http.StatusBadRequest)
		return
	}
	s.AddPeer(ctx, req.Addr)
	slog.Info("director API: peer added", "addr", req.Addr)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiPeerRemove(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("addr")
	if addr == "" {
		apiError(w, "addr query param required", http.StatusBadRequest)
		return
	}
	s.RemovePeer(addr)
	slog.Info("director API: peer removed", "addr", addr)
	apiJSON(w, map[string]string{"status": "ok"})
}
