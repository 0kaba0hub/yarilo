package backendapi

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/userdbinfo"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// userContext is the per-request storage state for a single user.
// Each request opens fresh namespace handles and Closes them; there is no
// cross-request state, so two admins acting on the same user never share
// half-resolved state.
type userContext struct {
	username string
	info     *mailbox.UserInfo
	owner    string

	// handles maps namespace slug ("personal", "shared", "public") to its
	// opened handle. Personal is always present after open(); shared/public
	// only when configured.
	handles map[string]*nsBundle
}

// nsBundle is one namespace's storage state, backed by the same per-user
// MailboxBackend/IndexBackend as a real session.
type nsBundle struct {
	spec     config.NamespaceConfig
	info     *mailbox.UserInfo
	box      mailbox.UserMailbox
	idx      mailbox.UserIndex
	location string
}

// openUserContext builds a context for username. The personal handle is
// opened eagerly; shared/public are opened lazily by ns(). Returns an error
// if the personal handle fails to open (typically a missing/unreadable home
// dir); shared/public failures are reported per-call via ns().
func (s *Server) openUserContext(username string) (*userContext, error) {
	return s.openUserContextInner(username, false)
}

// openUserContextReadOnly is like openUserContext but skips Init so no
// directories are created. When the user's home directory does not exist
// the personal namespace bundle is nil — callers must handle that case.
func (s *Server) openUserContextReadOnly(username string) (*userContext, error) {
	return s.openUserContextInner(username, true)
}

func (s *Server) openUserContextInner(username string, readOnly bool) (*userContext, error) {
	if username == "" {
		return nil, fmt.Errorf("backendapi/userctx: user required")
	}
	resolver := s.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	ui := resolver.UserInfo(username, "")
	var pui *protocol.UserInfo
	if s.opts.AuthClient != nil {
		var err error
		pui, err = s.opts.AuthClient.Userdb(context.Background(), username)
		if err != nil {
			return nil, fmt.Errorf("backendapi/userctx: userdb lookup: %w", err)
		}
		if pui == nil {
			return nil, fmt.Errorf("backendapi/userctx: user not found: %s", username)
		}
		userdbinfo.Apply(ui, pui, username)
	}
	uc := &userContext{
		username: username,
		info:     ui,
		owner:    fmt.Sprintf("yarilo-backend-api/%d/%s", os.Getpid(), username),
		handles:  make(map[string]*nsBundle),
	}

	personalSpec, ok := s.personalSpec()
	if !ok {
		personalSpec = config.NamespaceConfig{Type: "personal", Prefix: "", Separator: "/", List: "yes"}
	}
	personalMB := s.mailboxForUser(pui)
	var bundle *nsBundle
	var err error
	if readOnly {
		bundle, err = s.openNSReadOnly(personalSpec, ui, personalMB)
		if err != nil {
			return nil, fmt.Errorf("backendapi/userctx: open personal read-only: %w", err)
		}
	} else {
		bundle, err = s.openNS(personalSpec, ui, personalMB)
		if err != nil {
			return nil, fmt.Errorf("backendapi/userctx: open personal: %w", err)
		}
	}
	uc.handles["personal"] = bundle
	return uc, nil
}

// Close releases every opened namespace handle. Always safe to call.
func (uc *userContext) Close() {
	for _, h := range uc.handles {
		if h == nil {
			continue
		}
		if h.box != nil {
			_ = h.box.Close()
		}
		if h.idx != nil {
			_ = h.idx.Close()
		}
	}
	uc.handles = nil
}

// ns returns the bundle for the named namespace, lazily opening shared/public
// on first use. Returns an error when the namespace is unknown or has no
// location.
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
	var nsInfo *mailbox.UserInfo
	var err error
	if mailbox.PrefixIsOwnerTemplated(spec.Prefix) {
		// uc.info is the owner's resolved userdb identity (the caller opened the
		// context for the extracted owner), so the root and driver come from the
		// owner's mail_location; the template fills only the gaps -- the same
		// producer the IMAP path uses (#1142). base carries the deployment-wide
		// storage-name form (#1078, #1092), not the owner: it must not vary per
		// user the day the userdb starts answering those fields, so it comes from
		// the resolver's deployment defaults, not from uc.info.
		nsInfo, err = mailbox.StampOwnerLocation(uc.info, s.deploymentBase(), spec.Location, sepByte(spec.Separator))
		if err != nil {
			return nil, fmt.Errorf("backendapi/userctx: namespace %q: %w", name, err)
		}
	} else {
		loc, valid, perr := mailbox.ParseLocation(spec.Location, nil)
		if perr != nil {
			return nil, fmt.Errorf("backendapi/userctx: namespace %q location: %w", name, perr)
		}
		if !valid {
			return nil, fmt.Errorf("backendapi/userctx: namespace %q location empty", name)
		}
		// Username and Home alone are not enough: the consumers fall into
		// different defaults for everything else, so the mailbox backend looked
		// under <location>/Maildir while the ACL store looked in <location> and
		// every per-mailbox ACL call here answered "folder not found" (#1109).
		// One builder, shared with the IMAP path that had it right.
		nsInfo, err = mailbox.NamespaceUserInfo(uc.info, loc, spec.Separator)
		if err != nil {
			return nil, fmt.Errorf("backendapi/userctx: namespace %q: %w", name, err)
		}
	}
	b, err := s.openNS(spec, nsInfo, nil)
	if err != nil {
		return nil, fmt.Errorf("backendapi/userctx: open %q: %w", name, err)
	}
	uc.handles[name] = b
	return b, nil
}

// subsFileFor returns the subscription filename for a namespace bundle.
// Personal keeps the bare "subscriptions" filename so an upgrade does not
// orphan existing state; non-personal namespaces use "subscriptions-<slug>"
// siblings in their own home.
func subsFileFor(spec config.NamespaceConfig) string {
	return mailbox.NamespaceSubsFile(spec.Prefix, spec.Separator, spec.Type)
}

// mailboxForUser returns the MailboxBackend that matches the driver in
// pui.MailLocation. Falls back to opts.Mailbox when pui is nil, has no
// MailLocation, or the factory is not configured.
func (s *Server) mailboxForUser(pui *protocol.UserInfo) mailbox.MailboxBackend {
	if pui == nil || pui.MailLocation == "" {
		return s.opts.Mailbox
	}
	colon := strings.IndexByte(pui.MailLocation, ':')
	if colon < 0 {
		return s.opts.Mailbox
	}
	return s.mailboxForDriver(strings.ToLower(pui.MailLocation[:colon]))
}

// openNS instantiates one namespace's box+idx. mb overrides the backend when
// non-nil (per-user driver selection); nil falls back to the per-namespace or
// global default. Init runs to materialise the on-disk root.
func (s *Server) openNS(spec config.NamespaceConfig, ui *mailbox.UserInfo, mb mailbox.MailboxBackend) (*nsBundle, error) {
	return s.openNSInner(spec, ui, mb, false)
}

// openNSReadOnly is like openNS but skips Init so no directories are created.
// Returns (nil, nil) when the home directory does not exist; callers treat
// that as an empty namespace rather than an error.
func (s *Server) openNSReadOnly(spec config.NamespaceConfig, ui *mailbox.UserInfo, mb mailbox.MailboxBackend) (*nsBundle, error) {
	if ui.Home != "" {
		if _, err := os.Stat(ui.Home); os.IsNotExist(err) {
			return nil, nil
		}
	}
	return s.openNSInner(spec, ui, mb, true)
}

func (s *Server) openNSInner(spec config.NamespaceConfig, ui *mailbox.UserInfo, mb mailbox.MailboxBackend, skipInit bool) (*nsBundle, error) {
	if mb == nil {
		mb = s.mailboxBackendFor(spec, ui)
	}
	if mb == nil {
		return nil, fmt.Errorf("backendapi: no mailbox backend wired")
	}
	if s.opts.Index == nil {
		return nil, fmt.Errorf("backendapi: no index backend wired")
	}
	box := mb.OpenUser(ui)
	if !skipInit {
		if err := box.Init(); err != nil {
			return nil, fmt.Errorf("mailbox init: %w", err)
		}
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

// mailboxBackendFor returns the backend for a namespace: the per-namespace
// override wins, otherwise the global default.
func (s *Server) mailboxBackendFor(spec config.NamespaceConfig, ui *mailbox.UserInfo) mailbox.MailboxBackend {
	if override, ok := s.opts.NamespaceMailboxes[spec.Prefix]; ok && override != nil {
		return override
	}
	// Then the driver the namespace's own location names, not the
	// deployment-wide one. Selecting by prefix agreed with the location only
	// while every namespace used one format -- which is the case the Driver
	// field was added for (#1109).
	if ui != nil && ui.Driver != "" {
		return s.mailboxForDriver(ui.Driver)
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

// namespaceByName looks up a namespace by its slug. Returns false when no
// match.
func (s *Server) namespaceByName(name string) (config.NamespaceConfig, bool) {
	for _, spec := range s.opts.Namespaces {
		if slugFor(spec) == name {
			return spec, true
		}
	}
	if name == "personal" {
		return config.NamespaceConfig{Type: "personal", Prefix: "", Separator: "/", List: "yes"}, true
	}
	return config.NamespaceConfig{}, false
}

// deploymentBase is a user-less identity carrying only the deployment-wide
// storage-name form (StorageEscapeChar, SkipNFCNormalize). It is the base a
// namespace producer stamps so the same mailbox is spelled the same on disk for
// every user (#1078, #1092), independent of any one owner's userdb.
func (s *Server) deploymentBase() *mailbox.UserInfo {
	if s.opts.Resolver == nil {
		return nil
	}
	return s.opts.Resolver.UserInfo("", "")
}

// sepByte returns the namespace separator as a byte, defaulting to '/'.
func sepByte(sep string) byte {
	if sep == "" {
		return '/'
	}
	return sep[0]
}

// slugFor returns the canonical slug for a namespace spec, so the wire
// identifier used by admin requests matches the on-disk per-namespace
// filenames.
func slugFor(spec config.NamespaceConfig) string {
	if name := strings.ToLower(strings.TrimSuffix(spec.Prefix, "/")); name != "" {
		return name
	}
	return strings.ToLower(spec.Type)
}

// folderHome returns the per-namespace home directory used to root
// user-state files (subscriptions, special_use).
func (b *nsBundle) folderHome() string { return b.info.Home }

// folderControlRoot returns the root for per-folder control files
// (yarilo-uidlist, subscriptions). When CONTROL= is configured this
// differs from folderHome.
func (b *nsBundle) folderControlRoot() string {
	if b.info.ControlDir != "" {
		return b.info.ControlDir
	}
	if b.info.MailPath != "" {
		return b.info.MailPath
	}
	return b.info.Home
}

// lockOwner is the identifier shown in yarilo-locks BUSY reports for any lock
// this admin request acquires. Format mirrors the session owner so operators
// can correlate.
func (uc *userContext) lockOwner() string { return uc.owner }

// setActor records the acting identity for lock ownership, so an operator
// editing another account's namespace holds the lock under their own name, not
// the store owner's. No-op when actor is empty.
func (uc *userContext) setActor(actor string) {
	if actor == "" {
		return
	}
	uc.owner = fmt.Sprintf("yarilo-backend-api/%d/%s", os.Getpid(), actor)
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
