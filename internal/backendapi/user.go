package backendapi

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// registerUserRoutes wires the user admin surface.
//
//	info     local view (home, namespaces, per-namespace existence)
//	         plus the userdb block when AuthClient is configured.
//	usage    per-folder size + rollup, independent of userdb.
//	iterate  thin wrapper over pkg/authclient.Client.IterateUsers;
//	         503 when AuthClient is not configured.
func (s *Server) registerUserRoutes() {
	s.mux.Handle("POST /api/backend/user/info", s.middleware(s.handleUserInfo))
	s.mux.Handle("POST /api/backend/user/usage", s.middleware(s.handleUserUsage))
	s.mux.Handle("POST /api/backend/user/iterate", s.middleware(s.handleUserIterate))
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
	if req.User == "" {
		apiError(w, "backendapi/userctx: user required", http.StatusBadRequest)
		return
	}
	resolver := s.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	ui := resolver.UserInfo(req.User, "")

	nsEntries := []userNSEntry{}
	for _, spec := range s.opts.Namespaces {
		entry := userNSEntry{
			Name:   slugFor(spec),
			Type:   spec.Type,
			Prefix: spec.Prefix,
		}
		if spec.Type == "personal" {
			entry.Home = ui.Home
			entry.Exists = dirExists(ui.Home)
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
			Home:   ui.Home,
			Exists: dirExists(ui.Home),
		})
	}
	effectiveMailPath := ui.MailPath
	effectiveInboxPath := ui.InboxPath

	if s.opts.AuthClient != nil {
		pui, err := s.opts.AuthClient.Userdb(r.Context(), req.User)
		switch {
		case err != nil:
			slog.Warn("backendapi/user: userdb lookup failed", "user", req.User, "err", err)
			apiError(w, "userdb lookup: "+err.Error(), http.StatusServiceUnavailable)
			return
		case pui == nil:
			apiJSON(w, map[string]any{"error": "user not found: " + req.User})
			return
		default:
			if pui.MailPath != "" {
				mp := mailbox.ExpandHome(pui.MailPath, ui.Home)
				effectiveMailPath = mailbox.ExpandVars(strings.ReplaceAll(mp, "%h", ui.Home), req.User)
			} else if pui.MailLocation != "" {
				if colon := strings.IndexByte(pui.MailLocation, ':'); colon >= 0 {
					rest := pui.MailLocation[colon+1:]
					if next := strings.IndexByte(rest, ':'); next >= 0 {
						rest = rest[:next]
					}
					if rest != "" {
						mp := mailbox.ExpandHome(rest, ui.Home)
						effectiveMailPath = mailbox.ExpandVars(strings.ReplaceAll(mp, "%h", ui.Home), req.User)
					}
				}
			}
			if pui.InboxPath != "" {
				ip := mailbox.ExpandHome(pui.InboxPath, ui.Home)
				effectiveInboxPath = mailbox.ExpandVars(strings.ReplaceAll(ip, "%h", ui.Home), req.User)
			}
			if effectiveMailPath == "" {
				effectiveMailPath = ui.Home
			}
			if effectiveInboxPath == "" {
				effectiveInboxPath = effectiveMailPath
			}
			resp := map[string]any{
				"username":        ui.Username,
				"home":            ui.Home,
				"mail_path":       effectiveMailPath,
				"mail_inbox_path": effectiveInboxPath,
				"namespaces":      nsEntries,
				"userdb":          userInfoToJSON(pui),
			}
			apiJSON(w, resp)
			return
		}
	}

	if effectiveMailPath == "" {
		effectiveMailPath = ui.Home
	}
	if effectiveInboxPath == "" {
		effectiveInboxPath = effectiveMailPath
	}
	apiJSON(w, map[string]any{
		"username":        ui.Username,
		"home":            ui.Home,
		"mail_path":       effectiveMailPath,
		"mail_inbox_path": effectiveInboxPath,
		"namespaces":      nsEntries,
	})
}

// userInfoToJSON renders a protocol.UserInfo as the wire-friendly
// snake_case JSON object /user/info exposes. Zero-valued fields are
// omitted so the response stays compact — clients iterate over the
// keys that are present rather than special-casing the zero default.
// Sensitive fields (Password, CertName, PolicyResponse) are already
// stripped by the master-protocol wire serialiser before they cross
// the network, but we drop them defensively here too in case a
// future caller hands us an internally-populated UserInfo.
func userInfoToJSON(info *protocol.UserInfo) map[string]any {
	out := map[string]any{}
	setStr := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	setInt := func(k string, v int) {
		if v != 0 {
			out[k] = v
		}
	}
	setUint32 := func(k string, v uint32) {
		if v != 0 {
			out[k] = v
		}
	}
	setBool := func(k string, v bool) {
		if v {
			out[k] = v
		}
	}
	setList := func(k string, v []string) {
		if len(v) > 0 {
			out[k] = v
		}
	}

	setStr("original_user", info.OriginalUser)
	setStr("master_user", info.MasterUser)
	setStr("login_user", info.LoginUser)

	setUint32("uid", info.UID)
	setUint32("gid", info.GID)
	setStr("home", info.Home)
	setStr("chroot", info.Chroot)
	setStr("system_groups_user", info.SystemGroupsUser)
	setList("groups", info.Groups)
	setBool("client_cert_present", info.ClientCertPresent)

	setStr("mail_location", info.MailLocation)
	setStr("mail_path", info.MailPath)
	setStr("mail_inbox_path", info.InboxPath)
	setUint32("mail_uid", info.MailUID)
	setUint32("mail_gid", info.MailGID)
	setStr("mailbox_format", info.MailboxFormat)
	setStr("mail_attribute_dict", info.MailAttributeDict)

	setList("quota_rule", info.QuotaRules)
	setStr("quota_over_flag", info.QuotaOverFlag)

	setList("allow_nets", info.AllowNets)

	setBool("nologin", info.NoLogin)
	setBool("nodelay", info.NoDelay)
	setBool("noauthenticate", info.NoAuthenticate)
	setBool("pass_expired", info.PassExpired)
	setBool("nopassword", info.NoPassword)

	setBool("proxy", info.Proxy)
	setBool("proxy_maybe", info.ProxyMaybe)
	setStr("host", info.Host)
	setInt("port", info.Port)
	setStr("destuser", info.DestUser)
	setStr("proxy_mech", info.ProxyMech)
	setInt("proxy_timeout", info.ProxyTimeout)
	setBool("proxy_redirect_reauth", info.ProxyRedirectReauth)
	setBool("proxy_nopipelining", info.ProxyNoPipelining)
	setStr("ssl", info.SSL)
	setBool("starttls", info.StartTLS)

	setInt("mail_max_userip_connections", info.MailMaxUserIPConnections)
	setInt("mail_max_user_connections", info.MailMaxUserConnections)

	setStr("service", info.Service)
	setStr("local_name", info.LocalName)

	if len(info.Forward) > 0 {
		out["forward"] = info.Forward
	}
	if len(info.Extra) > 0 {
		out["extra"] = info.Extra
	}
	return out
}

// handleUserIterate streams every userdb-known username back as a
// sorted JSON array. 503 when AuthClient is not configured (no
// fallback enumeration source on backend-api alone — the caller is
// asked to point a yarilo-auth at AuthMasterAddr).
func (s *Server) handleUserIterate(w http.ResponseWriter, r *http.Request) {
	if s.opts.AuthClient == nil {
		apiError(w, "user/iterate requires backend_api.auth_master_addr (Phase AUTH-1 wiring)",
			http.StatusServiceUnavailable)
		return
	}
	users, err := s.opts.AuthClient.IterateUsers(r.Context())
	if err != nil {
		slog.Warn("backendapi/user: iterate failed", "err", err)
		apiError(w, "iterate: "+err.Error(), http.StatusBadGateway)
		return
	}
	sort.Strings(users)
	apiJSON(w, map[string]any{"users": users})
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
	uc, err := s.openUserContextReadOnly(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()

	var rows []usageFolder
	var totalMsgs uint32
	var totalSize uint64

	scanBundle := func(nsSlug string, bundle *nsBundle) {
		if bundle == nil {
			return
		}
		entries, err := bundle.box.ListFolders()
		if err != nil {
			return
		}
		folders := mailbox.SelectableNames(entries)
		sort.Strings(folders)
		for _, name := range folders {
			f, err := bundle.idx.OpenFolder(name, 0)
			if err != nil || f == nil {
				continue
			}
			msgs, err := bundle.idx.GetMessages(f.ID, nil)
			if err != nil {
				continue
			}
			var size uint64
			for _, m := range msgs {
				size += uint64(m.Size)
			}
			rows = append(rows, usageFolder{
				Namespace: nsSlug,
				Folder:    name,
				Messages:  uint32(len(msgs)),
				SizeBytes: size,
			})
			totalMsgs += uint32(len(msgs))
			totalSize += size
		}
	}

	for _, spec := range s.opts.Namespaces {
		bundle, err := uc.ns(s, slugFor(spec))
		if err != nil {
			continue
		}
		scanBundle(slugFor(spec), bundle)
	}
	if len(rows) == 0 {
		if bundle, err := uc.ns(s, "personal"); err == nil {
			scanBundle("personal", bundle)
		}
	}
	apiJSON(w, map[string]any{
		"user":             uc.info.Username,
		"folders":          rows,
		"total_messages":   totalMsgs,
		"total_size_bytes": totalSize,
	})
}
