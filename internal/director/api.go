package director

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/0kaba0hub/yarilo/internal/cluster/proto"
)

// boolParam reports whether query param name is a truthy flag ("1", "true", "yes",
// or present-but-empty like ?force). Absent or an explicit false value → false.
func boolParam(r *http.Request, name string) bool {
	if !r.URL.Query().Has(name) {
		return false
	}
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "", "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// intParam parses a non-negative integer query param; ok is false when absent or
// malformed so the caller can fall back to a default.
func intParam(r *http.Request, name string) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// StartAPI starts the HTTP admin API server on addr.
// token is required in the Authorization: Bearer header; empty string disables auth.
// allowedNets restricts access by client IP; nil/empty allows all.
func (s *Server) StartAPI(ctx context.Context, addr, token string, allowedNets []*net.IPNet) error {
	s.apiToken = token
	s.apiAddr = addr
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

	mux.Handle("GET /api/director/ring/topology", h(s.apiRingTopology))
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
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Tag      string `json:"tag"`
	Up       bool   `json:"up"`
	Vhosts   int    `json:"vhosts"`
	Sessions int    `json:"sessions"` // current active proxied sessions on this backend (this director's view)
}

func toBackendDTO(b backendDTOSource) backendDTO {
	v := b.Vhosts
	if v == 0 {
		v = 100
	}
	return backendDTO{b.IP, b.Port, b.Tag, b.Up, v, b.Sessions}
}

type backendDTOSource struct {
	IP       string
	Port     int
	Tag      string
	Up       bool
	Vhosts   int
	Sessions int
}

// resolveUserBackend answers "which backend is this user routed to" with the
// same precedence a login LOOKUP uses (#708): sticky userDir pin (if the backend
// is still up) → ring hash. user must already be normalized. Returns
// ("",0,"",false) when the ring is empty. sticky reports whether the answer came
// from an existing pin (vs a fresh hash). Read-only under hash; under
// least_sessions it pins a fresh user (the director owns placement).
func (s *Server) resolveUserBackend(user string) (ip string, port int, tag string, sticky bool) {
	if e := s.userDir.Get(user); e != nil && !e.Weak {
		if h, _, err := net.SplitHostPort(e.Host); err == nil {
			if b := s.ring.GetBackend(h); b != nil && b.Up {
				return b.IP, b.Port, b.Tag, true
			}
		}
	}
	// Fresh (unpinned) user. Under least_sessions the admin must NOT read a hash
	// pod the login would never assign — the director owns placement, so pin it
	// here too (assignAndPin). The admin path has no protocol, so pickBackend
	// skips level 1 and decides on total load. Under hash the read stays
	// side-effect-free (deterministic — admin hash == login hash).
	if s.assignmentPolicy() == policyLeastSessions {
		if b := s.assignAndPin(user, "", ""); b != nil {
			return b.IP, b.Port, b.Tag, false
		}
		return "", 0, "", false
	}
	if b := s.ring.LookupBackend(user); b != nil {
		return b.IP, b.Port, b.Tag, false
	}
	return "", 0, "", false
}

func (s *Server) apiStatus(w http.ResponseWriter, _ *http.Request) {
	backends := s.ring.Backends()
	sess := s.backendSessionCounts()
	bs := make([]backendDTO, len(backends))
	for i, b := range backends {
		bs[i] = toBackendDTO(backendDTOSource{b.IP, b.Port, b.Tag, b.Up, b.Vhosts, sess[b.IP]})
	}
	// Backends only — the director-membership (`peers`) list lives on the
	// dedicated GET /api/director/ring endpoint (`director ring status`).
	// status is the backend/routing plane; duplicating peers here made the
	// two commands overlap for no reason.
	apiJSON(w, map[string]any{"backends": bs})
}

func (s *Server) apiDump(w http.ResponseWriter, _ *http.Request) {
	type bDump struct {
		backendDTO
		LastUp   int64 `json:"last_up"`
		LastDown int64 `json:"last_down"`
	}
	backends := s.ring.Backends()
	sess := s.backendSessionCounts()
	bs := make([]bDump, len(backends))
	for i, b := range backends {
		bs[i] = bDump{
			backendDTO: toBackendDTO(backendDTOSource{b.IP, b.Port, b.Tag, b.Up, b.Vhosts, sess[b.IP]}),
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
	raw := r.URL.Query().Get("user")
	if raw != "" {
		user := s.normalizeUser(proto.TabUnescape(raw))

		// peek: pure introspection of the stored pin (#813). userDir.Get hashes
		// the username the SAME way entries are stored, so the CLI filter that
		// used to come back empty (hash-mismatch via the resolve path) now
		// matches. Crucially this has NO side effect — unlike the resolver
		// below it never assignAndPins an unpinned user, so an operator
		// inspecting the map cannot accidentally create pins.
		if r.URL.Query().Get("peek") != "" {
			e := s.userDir.Get(user)
			if e == nil {
				apiJSON(w, map[string]any{"user": raw, "pinned": false})
				return
			}
			ip, _, _ := net.SplitHostPort(e.Host)
			apiJSON(w, map[string]any{"user": raw, "pinned": true, "backend": ip, "host": e.Host, "weak": e.Weak})
			return
		}

		// Resolve to the SAME pod a login LOOKUP would:
		// sticky userDir pin → ring hash (#708). This matters for #792: a per-user
		// backend-api op (fts rescan, etc.) must hit the pod the user is
		// actually pinned to, or it becomes a second writer of that user's
		// index — the single-writer hazard co-location (#788) exists to avoid.
		// The director owns the assignment; the admin never picks a pod itself.
		// backendBaseForUser (yarctl) depends on this resolve+pin
		// behaviour — do NOT turn it into a pure read (use peek for that).
		ip, port, tag, sticky := s.resolveUserBackend(user)
		if ip == "" {
			apiError(w, "no backends available", http.StatusServiceUnavailable)
			return
		}
		apiJSON(w, map[string]any{"user": raw, "backend": ip, "port": port, "tag": tag, "sticky": sticky})
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
