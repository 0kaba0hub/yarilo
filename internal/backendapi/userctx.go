package backendapi

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// userContext is the per-request storage state for a single user.
//
// Mirrors the per-session wiring in internal/imap (dispatch.go) but
// strips the IMAP session machinery: an HTTP admin request only needs
// to open a namespace handle, do its work, then Close. There is no
// cross-request state — each request opens fresh handles so two
// admins acting on the same user never share half-resolved state.
type userContext struct {
	username string
	info     *mailbox.UserInfo
	owner    string

	// handles maps namespace identifier (the slug, e.g. "personal",
	// "shared", "public") to its opened handle. Personal is always
	// present after open(); shared/public only when configured.
	handles map[string]*nsBundle
}

// nsBundle is one namespace's storage state — backed by the same
// per-user MailboxBackend/IndexBackend as a real session.
type nsBundle struct {
	spec     config.NamespaceConfig
	info     *mailbox.UserInfo
	box      mailbox.UserMailbox
	idx      mailbox.UserIndex
	location string
}

// openUserContext builds a context for username. The personal handle
// is opened eagerly so any subsequent operation has it without an
// extra round-trip; shared/public are opened lazily by ns() when the
// caller asks for them.
//
// Returns an error if the personal handle fails to open (typically a
// missing/unreadable home dir). Shared/public failures are reported
// per-call via ns().
func (s *Server) openUserContext(username string) (*userContext, error) {
	if username == "" {
		return nil, fmt.Errorf("backendapi/userctx: user required")
	}
	resolver := s.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	ui := resolver.UserInfo(username, "")
	if s.opts.AuthClient != nil {
		pui, err := s.opts.AuthClient.Userdb(context.Background(), username)
		if err != nil {
			return nil, fmt.Errorf("backendapi/userctx: userdb lookup: %w", err)
		}
		if pui == nil {
			return nil, fmt.Errorf("backendapi/userctx: user not found: %s", username)
		}
		ui.MailPath = pui.MailPath
		ui.InboxPath = pui.InboxPath
	}
	uc := &userContext{
		username: username,
		info:     ui,
		owner:    fmt.Sprintf("yarilo-backend-api/%d/%s", os.Getpid(), username),
		handles:  make(map[string]*nsBundle),
	}

	personalSpec, ok := s.personalSpec()
	if !ok {
		personalSpec = config.NamespaceConfig{Type: "personal", Prefix: "", Separator: "/", List: true}
	}
	bundle, err := s.openNS(personalSpec, ui)
	if err != nil {
		return nil, fmt.Errorf("backendapi/userctx: open personal: %w", err)
	}
	uc.handles["personal"] = bundle
	return uc, nil
}

// Close releases every opened namespace handle. Always safe to call.
func (uc *userContext) Close() {
	for _, h := range uc.handles {
		if h.box != nil {
			_ = h.box.Close()
		}
		if h.idx != nil {
			_ = h.idx.Close()
		}
	}
	uc.handles = nil
}

// ns returns the bundle for the named namespace ("personal",
// "shared", "public", ...). Lazily opens shared/public on first use.
// Returns an error when the namespace is unknown or has no location.
func (uc *userContext) ns(s *Server, name string) (*nsBundle, error) {
	if name == "" {
		name = "personal"
	}
	if b, ok := uc.handles[name]; ok {
		return b, nil
	}
	spec, ok := s.namespaceByName(name)
	if !ok {
		return nil, fmt.Errorf("backendapi/userctx: namespace %q not configured", name)
	}
	if spec.Type == "personal" {
		// already opened in openUserContext
		return nil, fmt.Errorf("backendapi/userctx: personal namespace must be opened at construction")
	}
	if spec.Location == "" {
		return nil, fmt.Errorf("backendapi/userctx: namespace %q has no location", name)
	}
	loc, valid, err := mailbox.ParseLocation(spec.Location, nil)
	if err != nil {
		return nil, fmt.Errorf("backendapi/userctx: namespace %q location: %w", name, err)
	}
	if !valid {
		return nil, fmt.Errorf("backendapi/userctx: namespace %q location empty", name)
	}
	nsInfo := &mailbox.UserInfo{
		Username: uc.info.Username,
		Home:     loc.Path,
	}
	b, err := s.openNS(spec, nsInfo)
	if err != nil {
		return nil, fmt.Errorf("backendapi/userctx: open %q: %w", name, err)
	}
	uc.handles[name] = b
	return b, nil
}

// subsFileFor returns the subscription filename for a namespace bundle.
// Matches the convention used by internal/imap/dispatch.go: personal
// keeps the bare "subscriptions" filename so an upgrade does not
// orphan existing state; non-personal namespaces use
// "subscriptions-<slug>" siblings in their own home.
func subsFileFor(spec config.NamespaceConfig) string {
	if spec.Type == "personal" {
		return "subscriptions"
	}
	slug := strings.ToLower(strings.TrimSuffix(spec.Prefix, "/"))
	if slug == "" {
		slug = strings.ToLower(spec.Type)
	}
	return "subscriptions-" + slug
}

// openNS instantiates one namespace's box+idx using the per-namespace
// driver override when present, otherwise the global default. Init
// runs to materialise the on-disk root.
func (s *Server) openNS(spec config.NamespaceConfig, ui *mailbox.UserInfo) (*nsBundle, error) {
	mb := s.mailboxBackendFor(spec)
	if mb == nil {
		return nil, fmt.Errorf("backendapi: no mailbox backend wired")
	}
	if s.opts.Index == nil {
		return nil, fmt.Errorf("backendapi: no index backend wired")
	}
	box := mb.OpenUser(ui)
	if err := box.Init(); err != nil {
		return nil, fmt.Errorf("mailbox init: %w", err)
	}
	idx := s.opts.Index.OpenUser(ui)
	bundle := &nsBundle{
		spec:     spec,
		info:     ui,
		box:      box,
		idx:      idx,
		location: ui.Home,
	}
	return bundle, nil
}

// mailboxBackendFor mirrors session.mailboxBackendFor — namespace
// override wins, otherwise fall through to the global default.
func (s *Server) mailboxBackendFor(spec config.NamespaceConfig) mailbox.MailboxBackend {
	if override, ok := s.opts.NamespaceMailboxes[spec.Prefix]; ok && override != nil {
		return override
	}
	return s.opts.Mailbox
}

// personalSpec returns the first declared personal namespace.
func (s *Server) personalSpec() (config.NamespaceConfig, bool) {
	for _, spec := range s.opts.Namespaces {
		if spec.Type == "personal" {
			return spec, true
		}
	}
	return config.NamespaceConfig{}, false
}

// namespaceByName looks up a namespace by its slug — the same slug
// used by subsFileFor and the admin CLI. Returns false when no match.
func (s *Server) namespaceByName(name string) (config.NamespaceConfig, bool) {
	for _, spec := range s.opts.Namespaces {
		if slugFor(spec) == name {
			return spec, true
		}
	}
	if name == "personal" {
		return config.NamespaceConfig{Type: "personal", Prefix: "", Separator: "/", List: true}, true
	}
	return config.NamespaceConfig{}, false
}

// slugFor returns the canonical slug for a namespace spec. Mirrors
// nsSlug() in internal/imap/dispatch.go so the wire identifier used
// by admin requests matches the on-disk per-namespace filenames.
func slugFor(spec config.NamespaceConfig) string {
	if name := strings.ToLower(strings.TrimSuffix(spec.Prefix, "/")); name != "" {
		return name
	}
	return strings.ToLower(spec.Type)
}

// folderHome returns the per-namespace home directory used to root
// user-state files (subscriptions, special_use).
func (b *nsBundle) folderHome() string { return b.info.Home }

// folderIndexRoot returns the root for per-folder index files (yarilo.index*,
// yarilo-acl). When INDEX= is configured this differs from folderHome.
func (b *nsBundle) folderIndexRoot() string {
	if b.info.IndexDir != "" {
		return b.info.IndexDir
	}
	return b.info.Home
}

// folderControlRoot returns the root for per-folder control files
// (yarilo-uidlist, subscriptions). When CONTROL= is configured this
// differs from folderHome.
func (b *nsBundle) folderControlRoot() string {
	if b.info.ControlDir != "" {
		return b.info.ControlDir
	}
	return b.info.Home
}

// lockOwner is the identifier shown to yarilo-locks in BUSY reports
// for any lock acquired by this admin request. Format mirrors the
// session owner so operators can correlate.
func (uc *userContext) lockOwner() string { return uc.owner }

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
