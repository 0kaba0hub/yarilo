package director

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
)

// boolParam reports whether query param name is a truthy flag ("1", "true",
// "yes", or bare like ?force).
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
	Sessions int    `json:"sessions"` // active proxied sessions (this director's view)
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

// resolveUserBackend resolves a user with LOOKUP precedence: sticky pin (if
// its backend is still up) → ring hash. user must already be normalized.
// sticky reports whether an existing pin answered. Read-only under hash;
// under least_sessions a fresh user is pinned here.
func (s *Server) resolveUserBackend(user string) (ip string, port int, tag string, sticky bool) {
	if e := s.userDir.Get(user); e != nil && !e.Weak {
		if h, _, err := net.SplitHostPort(e.Host); err == nil {
			if b := s.ring.GetBackend(h); b != nil && b.Up {
				return b.IP, b.Port, b.Tag, true
			}
		}
	}
	// Fresh user: under least_sessions pin here too — the director owns
	// placement, and a hash read would name a pod the login never assigns.
	// Under hash the lookup is deterministic and side-effect-free.
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
	// Backends only; director membership lives on GET /api/director/ring.
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
	// Session records, one line each rather than a count per backend. A count
	// cannot answer the question a phantom raises -- whose record is this --
	// and the answer was previously reachable only by restarting a pod and
	// seeing what disappeared, which destroys the state being diagnosed
	// (#1393).
	type sDTO struct {
		ID    string `json:"id"`
		User  string `json:"user"`
		Host  string `json:"backend"`
		Proto string `json:"proto,omitempty"`
		// Local is true for a session this director owns, i.e. one whose login
		// connection is attached here. A replica belongs to another director's
		// run and is only counted, never kicked, from here.
		Local bool `json:"local"`
		// Origin names the director run a replica came from, in the same form
		// the purge logs use (field:port, incarnation included), so a dump and
		// a "purged session replicas of ..." line can be compared without
		// translating between two spellings.
		Origin string `json:"origin,omitempty"`
	}
	s.sessRecMu.RLock()
	ss := make([]sDTO, 0, len(s.sessById))
	for _, rec := range s.sessById {
		ss = append(ss, sDTO{
			ID: rec.id, User: rec.user, Host: rec.backend, Proto: rec.proto,
			Local: rec.cl != nil, Origin: rec.origin,
		})
	}
	s.sessRecMu.RUnlock()
	sort.Slice(ss, func(i, j int) bool { return ss[i].ID < ss[j].ID })

	apiJSON(w, map[string]any{"backends": bs, "users": us, "peers": s.ListPeers(), "sessions": ss})
}

func (s *Server) apiMap(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("user")
	if raw != "" {
		user := s.normalizeUser(proto.TabUnescape(raw))

		// peek: read the stored pin only. No side effects — never pins an
		// unpinned user, unlike the resolver below.
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

		// Resolve to the same pod a login LOOKUP would (sticky pin → ring
		// hash): a per-user backend op must hit the user's pod or it becomes
		// a second writer of that user's index. yarctl depends on the
		// resolve+pin behaviour — do not turn this into a pure read (use peek).
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
