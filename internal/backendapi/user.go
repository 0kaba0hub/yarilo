package backendapi

import (
	"net/http"
	"sort"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// registerUserRoutes wires the user admin surface.
//
// `info` returns what backend-api can resolve locally (home,
// configured namespaces, per-namespace existence). It deliberately
// does NOT call userdb — that requires a yarilo-auth client which
// backend-api does not currently embed. See TODO.md (BACKEND-API
// auth-aware lookups) for the follow-up.
//
// `usage` walks every folder in every implemented namespace, sums
// per-message sizes via UserMailbox.List, and returns a per-folder
// breakdown plus the rollup totals. Suitable for ad-hoc capacity
// inspection before QUOTA-1 ships.
func (s *Server) registerUserRoutes() {
	s.mux.Handle("POST /api/backend/user/info", s.middleware(s.handleUserInfo))
	s.mux.Handle("POST /api/backend/user/usage", s.middleware(s.handleUserUsage))
}

type userRequest struct {
	User string `json:"user"`
}

type userNSEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Prefix   string `json:"prefix"`
	Home     string `json:"home"`
	Location string `json:"location"`
	Exists   bool   `json:"exists"`
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()

	nsEntries := []userNSEntry{}
	for _, spec := range s.opts.Namespaces {
		entry := userNSEntry{
			Name:   slugFor(spec),
			Type:   spec.Type,
			Prefix: spec.Prefix,
		}
		if spec.Type == "personal" {
			entry.Home = uc.info.Home
			entry.Exists = dirExists(uc.info.Home)
		} else if spec.Location != "" {
			loc, ok, err := mailbox.ParseLocation(spec.Location, nil)
			if err == nil && ok {
				entry.Home = loc.Path
				entry.Location = spec.Location
				entry.Exists = dirExists(loc.Path)
			}
		}
		nsEntries = append(nsEntries, entry)
	}
	if len(nsEntries) == 0 {
		nsEntries = append(nsEntries, userNSEntry{
			Name:   "personal",
			Type:   "personal",
			Home:   uc.info.Home,
			Exists: dirExists(uc.info.Home),
		})
	}
	apiJSON(w, map[string]any{
		"username":   uc.info.Username,
		"home":       uc.info.Home,
		"namespaces": nsEntries,
	})
}

type usageFolder struct {
	Namespace string `json:"namespace"`
	Folder    string `json:"folder"`
	Messages  uint32 `json:"messages"`
	SizeBytes uint64 `json:"size_bytes"`
}

func (s *Server) handleUserUsage(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()

	var rows []usageFolder
	var totalMsgs uint32
	var totalSize uint64
	for _, spec := range s.opts.Namespaces {
		bundle, err := uc.ns(s, slugFor(spec))
		if err != nil {
			continue
		}
		folders, err := bundle.box.ListFolders()
		if err != nil {
			continue
		}
		sort.Strings(folders)
		for _, name := range folders {
			msgs, err := bundle.box.List(name)
			if err != nil {
				continue
			}
			var size uint64
			for _, m := range msgs {
				size += uint64(m.Size)
			}
			rows = append(rows, usageFolder{
				Namespace: slugFor(spec),
				Folder:    name,
				Messages:  uint32(len(msgs)),
				SizeBytes: size,
			})
			totalMsgs += uint32(len(msgs))
			totalSize += size
		}
	}
	if len(rows) == 0 {
		bundle, err := uc.ns(s, "personal")
		if err == nil {
			folders, err := bundle.box.ListFolders()
			if err == nil {
				sort.Strings(folders)
				for _, name := range folders {
					msgs, err := bundle.box.List(name)
					if err != nil {
						continue
					}
					var size uint64
					for _, m := range msgs {
						size += uint64(m.Size)
					}
					rows = append(rows, usageFolder{
						Namespace: "personal",
						Folder:    name,
						Messages:  uint32(len(msgs)),
						SizeBytes: size,
					})
					totalMsgs += uint32(len(msgs))
					totalSize += size
				}
			}
		}
	}
	apiJSON(w, map[string]any{
		"user":             uc.info.Username,
		"folders":          rows,
		"total_messages":   totalMsgs,
		"total_size_bytes": totalSize,
	})
}
