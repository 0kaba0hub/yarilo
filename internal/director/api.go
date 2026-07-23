package director

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// StartAPI starts the HTTP admin API server on addr.
// token is required in the Authorization: Bearer header; empty string disables auth.
// allowedNets restricts access by client IP; nil/empty allows all.
func (s *Server) StartAPI(ctx context.Context, addr, token string, allowedNets []*net.IPNet) error {
	mux := http.NewServeMux()
	h := func(fn http.HandlerFunc) http.Handler { return s.apiMiddleware(token, allowedNets, fn) }

	mux.Handle("GET /api/director/status", h(s.apiStatus))
	mux.Handle("GET /api/director/dump", h(s.apiDump))
	mux.Handle("GET /api/director/map", h(s.apiMap))

	mux.Handle("GET /api/director/backends", h(s.apiBackendList))
	mux.Handle("POST /api/director/backends", h(s.apiBackendAdd))
	mux.Handle("PATCH /api/director/backends/{ip}", h(s.apiBackendUpdate))
	mux.Handle("DELETE /api/director/backends/{ip}", h(s.apiBackendRemove))
	mux.Handle("POST /api/director/backends/{ip}/up", h(s.apiBackendUp))
	mux.Handle("POST /api/director/backends/{ip}/down", h(s.apiBackendDown))
	mux.Handle("POST /api/director/backends/{ip}/flush", h(s.apiBackendFlush))

	mux.Handle("POST /api/director/users/{user}/move", h(s.apiUserMove))
	mux.Handle("POST /api/director/users/{user}/kick", h(s.apiUserKick))

	mux.Handle("GET /api/director/ring", h(s.apiPeerList))
	mux.Handle("POST /api/director/ring", h(s.apiPeerAdd))
	mux.Handle("DELETE /api/director/ring", h(s.apiPeerRemove))

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	slog.Info("director: API listening", "addr", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("director API: %w", err)
	}
	return nil
}

// apiMiddleware chains IP whitelist and Bearer token checks.
func (s *Server) apiMiddleware(token string, nets []*net.IPNet, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(nets) > 0 {
			clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(clientIP)
			allowed := false
			for _, n := range nets {
				if ip != nil && n.Contains(ip) {
					allowed = true
					break
				}
			}
			if !allowed {
				apiError(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		if token != "" {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != token {
				apiError(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	})
}

func apiJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func apiError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

type backendDTO struct {
	IP     string `json:"ip"`
	Port   int    `json:"port"`
	Tag    string `json:"tag"`
	Up     bool   `json:"up"`
	Vhosts int    `json:"vhosts"`
}

func toBackendDTO(b backendDTOSource) backendDTO {
	v := b.Vhosts
	if v == 0 {
		v = 100
	}
	return backendDTO{b.IP, b.Port, b.Tag, b.Up, v}
}

type backendDTOSource struct {
	IP     string
	Port   int
	Tag    string
	Up     bool
	Vhosts int
}

func (s *Server) apiStatus(w http.ResponseWriter, _ *http.Request) {
	backends := s.ring.Backends()
	bs := make([]backendDTO, len(backends))
	for i, b := range backends {
		bs[i] = toBackendDTO(backendDTOSource{b.IP, b.Port, b.Tag, b.Up, b.Vhosts})
	}
	apiJSON(w, map[string]any{"backends": bs, "peers": s.ListPeers()})
}

func (s *Server) apiDump(w http.ResponseWriter, _ *http.Request) {
	type bDump struct {
		backendDTO
		LastUp   int64 `json:"last_up"`
		LastDown int64 `json:"last_down"`
	}
	backends := s.ring.Backends()
	bs := make([]bDump, len(backends))
	for i, b := range backends {
		bs[i] = bDump{
			backendDTO: toBackendDTO(backendDTOSource{b.IP, b.Port, b.Tag, b.Up, b.Vhosts}),
			LastUp:     b.LastUp,
			LastDown:   b.LastDown,
		}
	}
	type uDTO struct {
		Hash      uint32 `json:"hash"`
		Host      string `json:"host"`
		Weak      bool   `json:"weak"`
		ExpiresAt int64  `json:"expires_at"`
	}
	users := s.userDir.Snapshot()
	us := make([]uDTO, len(users))
	for i, u := range users {
		us[i] = uDTO{u.Hash, u.Host, u.Weak, u.ExpiresAt.Unix()}
	}
	apiJSON(w, map[string]any{"backends": bs, "users": us, "peers": s.ListPeers()})
}

func (s *Server) apiMap(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	if user != "" {
		b := s.ring.LookupBackend(user)
		if b == nil {
			apiError(w, "no backends available", http.StatusServiceUnavailable)
			return
		}
		apiJSON(w, map[string]any{"user": user, "backend": b.IP, "port": b.Port, "tag": b.Tag})
		return
	}
	type uDTO struct {
		Hash uint32 `json:"hash"`
		Host string `json:"host"`
		Weak bool   `json:"weak"`
	}
	users := s.userDir.Snapshot()
	us := make([]uDTO, len(users))
	for i, u := range users {
		us[i] = uDTO{u.Hash, u.Host, u.Weak}
	}
	apiJSON(w, map[string]any{"users": us})
}
