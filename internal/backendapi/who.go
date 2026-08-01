package backendapi

import (
	"crypto/tls"
	"net/http"
	"sort"
	"time"

	"github.com/0kaba0hub/yarilo/internal/warden"
)

// registerWhoRoutes registers active-session listing routes.
// Dials warden per request; no long-lived pool needed for an
// admin tool.
func (s *Server) registerWhoRoutes() {
	s.mux.Handle("POST /api/backend/who", s.middleware(s.handleWho))
	s.mux.Handle("POST /api/backend/who/count", s.middleware(s.handleWhoCount))
}

type whoRequest struct {
	Service string `json:"service"`
	User    string `json:"user"`
	GroupBy string `json:"group_by"`
	// All disables the default local-backend scoping: cluster-wide
	// view with each session's Backend visible.
	All bool `json:"all"`
}

type whoSessionOut struct {
	ID          string `json:"id"`
	User        string `json:"user"`
	IP          string `json:"ip"`
	Service     string `json:"service"`
	ConnectedAt string `json:"connected_at"`
	// Folder is the SELECTed IMAP mailbox; empty if none or not IMAP.
	Folder string `json:"folder,omitempty"`
	// Backend is the backend pod IP the session routed to.
	Backend string `json:"backend,omitempty"`
}

// filterLocalBackend keeps only sessions routed to podIP.
// Empty podIP cannot scope; the list is returned unchanged.
func filterLocalBackend(sessions []warden.SessionInfo, podIP string) []warden.SessionInfo {
	if podIP == "" {
		return sessions
	}
	out := make([]warden.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		if s.Backend == podIP {
			out = append(out, s)
		}
	}
	return out
}

type whoGroupOut struct {
	User     string          `json:"user"`
	Total    int             `json:"total"`
	Sessions []whoSessionOut `json:"sessions"`
}

func (s *Server) handleWho(w http.ResponseWriter, r *http.Request) {
	var req whoRequest
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if s.opts.WardenAddr == "" {
		apiError(w, "yarilo-warden endpoint not configured", http.StatusNotImplemented)
		return
	}
	c, err := warden.Dial(s.opts.WardenAddr, s.opts.WardenTLS, 5*time.Second)
	if err != nil {
		apiError(w, "warden dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer c.Close()

	sessions, err := c.Who(warden.WhoFilter{Service: req.Service, User: req.User})
	if err != nil {
		apiError(w, "warden who: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Default scope: only sessions routed to this backend.
	if !req.All {
		sessions = filterLocalBackend(sessions, s.opts.PodIP)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].User != sessions[j].User {
			return sessions[i].User < sessions[j].User
		}
		return sessions[i].ConnectedAt.Before(sessions[j].ConnectedAt)
	})

	// Grouping by user gives the per-mailbox view.
	if req.GroupBy == "" {
		req.GroupBy = "user"
	}

	switch req.GroupBy {
	case "none":
		out := make([]whoSessionOut, 0, len(sessions))
		for _, sess := range sessions {
			out = append(out, formatSession(sess))
		}
		apiJSON(w, map[string]any{
			"total":    len(sessions),
			"sessions": out,
		})
	default:
		grouped := map[string]*whoGroupOut{}
		order := []string{}
		for _, sess := range sessions {
			g, ok := grouped[sess.User]
			if !ok {
				g = &whoGroupOut{User: sess.User}
				grouped[sess.User] = g
				order = append(order, sess.User)
			}
			g.Sessions = append(g.Sessions, formatSession(sess))
			g.Total++
		}
		out := make([]*whoGroupOut, 0, len(order))
		for _, u := range order {
			out = append(out, grouped[u])
		}
		apiJSON(w, map[string]any{
			"total":  len(sessions),
			"groups": out,
		})
	}
}

// handleWhoCount returns aggregated session counts. Same filters
// as /who; by="" total only, by="protocol" service→count,
// by="user" user→count.
func (s *Server) handleWhoCount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string `json:"service"`
		User    string `json:"user"`
		By      string `json:"by"`
		All     bool   `json:"all"`
	}
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if s.opts.WardenAddr == "" {
		apiError(w, "yarilo-warden endpoint not configured", http.StatusNotImplemented)
		return
	}
	c, err := warden.Dial(s.opts.WardenAddr, s.opts.WardenTLS, 5*time.Second)
	if err != nil {
		apiError(w, "warden dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer c.Close()

	sessions, err := c.Who(warden.WhoFilter{Service: req.Service, User: req.User})
	if err != nil {
		apiError(w, "warden who: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !req.All {
		sessions = filterLocalBackend(sessions, s.opts.PodIP)
	}
	resp := map[string]any{
		"total":   len(sessions),
		"service": req.Service,
		"user":    req.User,
	}
	switch req.By {
	case "protocol", "service":
		byProto := map[string]int{}
		for _, sess := range sessions {
			byProto[sess.Service]++
		}
		resp["by_protocol"] = byProto
	case "user":
		byUser := map[string]int{}
		for _, sess := range sessions {
			byUser[sess.User]++
		}
		resp["by_user"] = byUser
	case "":
		// total only — already on resp
	default:
		apiError(w, `by must be "" | "protocol" | "user"`, http.StatusBadRequest)
		return
	}
	apiJSON(w, resp)
}

func formatSession(s warden.SessionInfo) whoSessionOut {
	return whoSessionOut{
		ID:          s.ID,
		User:        s.User,
		IP:          s.IP,
		Service:     s.Service,
		ConnectedAt: s.ConnectedAt.UTC().Format(time.RFC3339),
		Folder:      s.Folder,
		Backend:     s.Backend,
	}
}

// WardenEndpoint is the slice of Options consumed by the who route;
// separate type so CLI/tests avoid the full Server.
type WardenEndpoint struct {
	Addr      string
	TLSConfig *tls.Config
}
