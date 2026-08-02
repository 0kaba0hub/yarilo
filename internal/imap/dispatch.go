package imap

import (
	"fmt"
	"os"
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// nsHandle is the per-namespace storage state attached to a session.
// Declared-only namespaces (other_users) have nil box/idx/subs and the
// dispatcher routes them to a "not yet implemented" NO response.
type nsHandle struct {
	// name is the logical namespace identifier ("personal", "shared",
	// "public", "other"); used for logs and the subscription filename.
	name string
	// spec carries the wire-protocol details (prefix, separator, type).
	spec NamespaceSpec
	// location is the resolved storage path; empty for backend-less handles.
	location string
	// box / idx are the per-user storage handles; nil when declared-only.
	box mailbox.UserMailbox
	idx mailbox.UserIndex
	// subs is the per-namespace subscription store. Personal keeps the
	// filename "subscriptions" so upgrades preserve existing state;
	// shared/public use "subscriptions-<ns>" siblings.
	subs *subs.Store
	// acl is the per-namespace ACL store backed by yarilo-acl files inside
	// each folder's index dir; nil for declared-only namespaces.
	acl *acl.Store
	// userInfo is who the handle was opened for. Shared/public use a
	// synthetic UserInfo whose Home is the namespace root.
	userInfo *mailbox.UserInfo
}

// implemented reports whether this namespace has working backends.
func (h *nsHandle) implemented() bool { return h != nil && h.box != nil && h.idx != nil }

// fullName returns the wire-protocol mailbox name for a folder in this
// namespace. Inverse of dispatch().
func (h *nsHandle) fullName(relName string) string {
	if h.spec.Prefix == "" {
		return relName
	}
	return h.spec.Prefix + relName
}

// openHandles constructs the per-namespace handles for a session at login
// time, keyed by namespace prefix. The personal handle is always created.
// Other-class handles are skipped (declared in the NAMESPACE response but
// "not implemented" on access); shared/public open only when their
// location is configured.
func (s *session) openHandles(personalUI *mailbox.UserInfo) (map[string]*nsHandle, *nsHandle, error) {
	specs := s.srv.opts.Namespaces
	if len(specs) == 0 {
		specs = defaultNamespaces
	}

	owner := s.owner(personalUI.Username)
	out := make(map[string]*nsHandle, len(specs))
	var primary *nsHandle

	for _, spec := range specs {
		switch spec.Type {
		case NamespacePersonal:
			h, err := s.openHandle(spec, "personal", personalUI, owner, "subscriptions")
			if err != nil {
				return nil, nil, fmt.Errorf("imap: open personal namespace: %w", err)
			}
			out[spec.Prefix] = h
			if primary == nil {
				primary = h
			}
		case NamespaceShared, NamespaceOther: //nolint:exhaustive
			if spec.Location == "" {
				// Declared without storage: visible in NAMESPACE, but
				// SELECT under its prefix returns NO.
				continue
			}
			loc, ok, err := mailbox.ParseLocation(spec.Location, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("imap: %s namespace location: %w", spec.Type, err)
			}
			if !ok {
				continue
			}
			subsFile := "subscriptions-" + nsSlug(spec)
			// Set MailPath (not just Home) plus driver and modifiers so the
			// mailbox backend, fileindex and ACL store all resolve to the same
			// root; otherwise the ACL store (falls back to Home) and a maildir
			// backend (falls back to Home/Maildir) disagree.
			ui := &mailbox.UserInfo{
				Username:    personalUI.Username,
				Home:        loc.Path,
				MailPath:    loc.Path,
				Driver:      loc.Driver,
				IndexDir:    loc.IndexDir,
				VolatileDir: loc.VolatileDir,
				ControlDir:  loc.ControlDir,
				AltDir:      loc.AltDir,
			}
			h, err := s.openHandle(spec, nsSlug(spec), ui, owner, subsFile)
			if err != nil {
				return nil, nil, fmt.Errorf("imap: open %s namespace: %w", spec.Type, err)
			}
			h.location = loc.Path
			out[spec.Prefix] = h
		}
	}

	if primary == nil {
		// No personal namespace configured: fall back to a single
		// empty-prefix personal namespace.
		fallback := NamespaceSpec{Type: NamespacePersonal, Prefix: "", Separator: '.', List: true}
		h, err := s.openHandle(fallback, "personal", personalUI, owner, "subscriptions")
		if err != nil {
			return nil, nil, fmt.Errorf("imap: open fallback personal namespace: %w", err)
		}
		out[""] = h
		primary = h
	}
	return out, primary, nil
}

// openHandle wires one namespace's box + idx + subs. The mailbox backend
// comes from NamespaceMailboxes[spec.Prefix] when an override is present
// (per-namespace driver mixing), otherwise from global Options.Mailbox.
func (s *session) openHandle(spec NamespaceSpec, name string, ui *mailbox.UserInfo, owner, subsFile string) (*nsHandle, error) {
	ui.Separator = string(spec.Separator)
	mb := s.mailboxBackendFor(spec)
	box := mb.OpenUser(ui)
	if err := box.Init(); err != nil {
		return nil, fmt.Errorf("mailbox init: %w", err)
	}
	idx := s.srv.opts.Index.OpenUser(ui)
	// Subscriptions live in the control root: ControlDir when set, else
	// MailPath, falling back to Home.
	subsRoot := ui.Home
	if ui.MailPath != "" {
		subsRoot = ui.MailPath
	}
	if ui.ControlDir != "" {
		subsRoot = ui.ControlDir
	}
	store := subs.New(subsRoot, subsFile, ui.Username, owner, s.srv.opts.Locker)
	// acl_defaults_from_inbox applies to personal/shared namespaces only.
	defaultsFromInbox := s.srv.opts.ACLDefaultsFromInbox &&
		(spec.Type == NamespacePersonal || spec.Type == NamespaceShared)
	aclStore := acl.New(ui.Home, ui.MailPath, ui.Driver, ui.Separator, ui.Username, owner, acl.Policy{
		DefaultsFromInbox: defaultsFromInbox,
		GlobalsOnly:       s.srv.opts.ACLGlobalsOnly,
		CacheTTL:          s.srv.opts.ACLCacheTTL,
		Global:            s.srv.opts.ACLGlobal,
	}, s.srv.opts.Locker)
	return &nsHandle{
		name:     name,
		spec:     spec,
		box:      box,
		idx:      idx,
		subs:     store,
		acl:      aclStore,
		userInfo: ui,
	}, nil
}

// mailboxBackendFor returns the per-namespace MailboxBackend, in priority
// order: NamespaceMailboxes override for the prefix, per-user
// personalMailbox, then global Options.Mailbox.
func (s *session) mailboxBackendFor(spec NamespaceSpec) mailbox.MailboxBackend {
	if override, ok := s.srv.opts.NamespaceMailboxes[spec.Prefix]; ok && override != nil {
		return override
	}
	if spec.Type == NamespacePersonal && s.personalMailbox != nil {
		return s.personalMailbox
	}
	return s.srv.opts.Mailbox
}

// nsSlug returns a short identifier for a namespace spec, used in
// per-namespace filenames (subscriptions-<slug>) and log fields.
func nsSlug(spec NamespaceSpec) string {
	if name := strings.TrimSuffix(spec.Prefix, "/"); name != "" {
		return strings.ToLower(name)
	}
	return string(spec.Type)
}

// dispatch resolves a wire-protocol mailbox name to its namespace handle
// plus the namespace-relative name (prefix stripped). "INBOX" is always
// personal; otherwise the longest matching non-empty prefix wins. A name
// under a declared-but-unimplemented prefix returns a NO-typed
// *imaplib.Error; nothing matching falls back to the personal handle with
// the name unchanged.
func (s *session) dispatch(name string) (*nsHandle, string, error) {
	if strings.EqualFold(name, "INBOX") {
		return s.primary, "INBOX", nil
	}

	// Reject declared-but-unimplemented namespaces first. Iterate the wire
	// spec list to catch other_users prefixes that opened no handle.
	specs := s.srv.opts.Namespaces
	if len(specs) == 0 {
		specs = defaultNamespaces
	}
	for _, spec := range specs {
		if spec.Type != NamespaceOther {
			continue
		}
		if spec.Prefix != "" && strings.HasPrefix(name, spec.Prefix) {
			return nil, "", &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Text: "Other Users namespace requires ACL-1 (RFC 4314) and NS-3 (cross-pod routing); not yet implemented",
			}
		}
	}

	// Longest non-empty prefix match wins.
	var bestPrefix string
	var best *nsHandle
	for prefix, h := range s.namespaces {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(name, prefix) && len(prefix) > len(bestPrefix) {
			best = h
			bestPrefix = prefix
		}
	}
	if best != nil {
		return best, strings.TrimPrefix(name, bestPrefix), nil
	}
	return s.primary, name, nil
}

// orderedHandles returns the implemented namespace handles in stable order
// (personal first, then by prefix ascending) so cross-namespace LIST
// traversal yields consistent listings across reused connections.
func (s *session) orderedHandles() []*nsHandle {
	out := make([]*nsHandle, 0, len(s.namespaces))
	for _, h := range s.namespaces {
		if !h.implemented() {
			continue
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		// Personal (empty prefix) first.
		pi, pj := out[i].spec.Prefix, out[j].spec.Prefix
		if pi == "" && pj != "" {
			return true
		}
		if pj == "" && pi != "" {
			return false
		}
		return pi < pj
	})
	return out
}

// closeHandles releases per-namespace resources at session teardown.
func (s *session) closeHandles() {
	for _, h := range s.namespaces {
		if h.box != nil {
			h.box.Close() //nolint:errcheck
		}
		if h.idx != nil {
			h.idx.Close() //nolint:errcheck
		}
	}
}

// owner builds the yarilo-locks owner string for diagnostics.
func (s *session) owner(username string) string {
	return fmt.Sprintf("yarilo-imap/%d/%s", os.Getpid(), username)
}
