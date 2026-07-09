package imap

import (
	"fmt"
	"os"
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/0kaba0hub/yarilo/internal/userstate/acl"
	"github.com/0kaba0hub/yarilo/internal/userstate/subs"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// nsHandle is the per-namespace storage state attached to a session.
// One handle per configured namespace (personal, shared, public, ...);
// "other_users"-class namespaces declared via NAMESPACE but not yet
// implemented have nil box/idx/subs — dispatcher routes them to a
// "not yet implemented" NO response.
type nsHandle struct {
	// name is the logical namespace identifier ("personal", "shared",
	// "public", "other"). Used for log lines and the per-namespace
	// subscription filename.
	name string
	// spec carries the wire-protocol details (prefix, separator, type).
	spec NamespaceSpec
	// location is the resolved physical storage URL after varexpand.
	// Empty for handles that have no backend (other_users in NS-1b).
	location string
	// box / idx are the per-user storage handles for THIS namespace.
	// Nil when the namespace is declared-only (other_users).
	box mailbox.UserMailbox
	idx mailbox.UserIndex
	// subs is the per-namespace subscription store. Personal keeps the
	// pre-v1.21 filename "subscriptions" so upgrades preserve existing
	// state; shared/public use "subscriptions-<ns>" siblings in the
	// user's home so each namespace tracks its own SUBSCRIBE state.
	subs *subs.Store
	// acl is the per-namespace ACL store backed by yarilo-acl files
	// inside each folder's index dir. Created at openHandle time; the
	// SessionACL implementation dispatches GETACL/SETACL/DELETEACL/
	// MYRIGHTS/LISTRIGHTS through it. Stays nil only for declared-only
	// namespaces (no backend, no ACL state to manage).
	acl *acl.Store
	// userInfo captures who the handle was opened for. Personal uses
	// the authenticated user's UserInfo; shared/public use a synthetic
	// UserInfo whose Home is the namespace root.
	userInfo *mailbox.UserInfo
}

// implemented reports whether this namespace has working backends.
// Other Users in NS-1b is declared but not implemented.
func (h *nsHandle) implemented() bool { return h != nil && h.box != nil && h.idx != nil }

// fullName returns the wire-protocol mailbox name for a folder living
// in this namespace. Inverse of dispatch().
func (h *nsHandle) fullName(relName string) string {
	if h.spec.Prefix == "" {
		return relName
	}
	return h.spec.Prefix + relName
}

// openHandles constructs the per-namespace handles for a session at
// login time. The personal handle is always created (with userInfo as
// its Home). Other-class handles are skipped — they are declared in
// the NAMESPACE response by the session.Namespace() method but the
// dispatcher returns "not implemented" on access. Shared/public open
// only when their location: is configured.
//
// Returns the handle map keyed by namespace prefix.
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
				// Declared without storage — skip handle; sessions
				// see this namespace in NAMESPACE responses but
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
			// loc.Path is the namespace's mailbox store root. Set MailPath
			// (not just Home) and the driver + modifiers so the mailbox
			// backend, the fileindex and the ACL store all resolve to the
			// same root — otherwise the ACL store (which falls back to Home)
			// and a maildir backend (which falls back to Home/Maildir) would
			// disagree, and dbox namespaces would get the maildir ACL layout.
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
		// Operator configured no personal namespace — fall back so
		// existing pre-v1.21 single-namespace clients keep working.
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

// openHandle wires one namespace's box + idx + subs. The mailbox
// backend is chosen from NamespaceMailboxes[spec.Prefix] when an
// override is present (per-namespace driver mixing, e.g.
// personal=maildir + shared=mdbox); otherwise falls back to the
// global Options.Mailbox. The index backend is uniform — fileindex
// works against any storage driver.
func (s *session) openHandle(spec NamespaceSpec, name string, ui *mailbox.UserInfo, owner, subsFile string) (*nsHandle, error) {
	// The backends convert this namespace's IMAP hierarchy separator to their
	// on-disk separator (maildir "." flat, dbox "/" nested).
	ui.Separator = string(spec.Separator)
	mb := s.mailboxBackendFor(spec)
	box := mb.OpenUser(ui)
	if err := box.Init(); err != nil {
		return nil, fmt.Errorf("mailbox init: %w", err)
	}
	idx := s.srv.opts.Index.OpenUser(ui)
	// Subscriptions live in the control root: mail_control_path (ControlDir)
	// when set, otherwise the mail root (MailPath), falling back to Home.
	subsRoot := ui.Home
	if ui.MailPath != "" {
		subsRoot = ui.MailPath
	}
	if ui.ControlDir != "" {
		subsRoot = ui.ControlDir
	}
	store := subs.New(subsRoot, subsFile, ui.Username, owner, s.srv.opts.Locker)
	// acl_defaults_from_inbox applies to private/shared namespaces only.
	defaultsFromInbox := s.srv.opts.ACLDefaultsFromInbox &&
		(spec.Type == NamespacePersonal || spec.Type == NamespaceShared)
	aclStore := acl.New(ui.Home, ui.MailPath, ui.Driver, ui.Separator, ui.Username, owner, defaultsFromInbox, s.srv.opts.Locker)
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

// mailboxBackendFor returns the per-namespace MailboxBackend. Priority:
// 1. explicit NamespaceMailboxes override for the prefix
// 2. per-user personalMailbox (set when mail_location carries a driver)
// 3. global Options.Mailbox
func (s *session) mailboxBackendFor(spec NamespaceSpec) mailbox.MailboxBackend {
	if override, ok := s.srv.opts.NamespaceMailboxes[spec.Prefix]; ok && override != nil {
		return override
	}
	if spec.Type == NamespacePersonal && s.personalMailbox != nil {
		return s.personalMailbox
	}
	return s.srv.opts.Mailbox
}

// nsSlug normalises a namespace spec into a short identifier used for
// per-namespace filenames (subscriptions-<slug>) and log fields.
func nsSlug(spec NamespaceSpec) string {
	if name := strings.TrimSuffix(spec.Prefix, "/"); name != "" {
		return strings.ToLower(name)
	}
	return string(spec.Type)
}

// dispatch resolves a wire-protocol mailbox name to its namespace
// handle plus the namespace-relative name (prefix stripped).
//
// Rules:
//   - "INBOX" (case-insensitive) always lives in the personal namespace.
//   - The longest matching non-empty prefix among configured handles wins.
//   - When a name matches a DECLARED but UNIMPLEMENTED namespace prefix
//     (e.g. other_users in NS-1b), returns a NO-typed *imaplib.Error
//     instead of routing — callers surface it directly.
//   - When nothing matches, falls back to the personal handle with the
//     name unchanged. Preserves pre-v1.21 single-namespace behaviour.
func (s *session) dispatch(name string) (*nsHandle, string, error) {
	if strings.EqualFold(name, "INBOX") {
		return s.primary, "INBOX", nil
	}

	// First check: does the name belong to a declared but
	// unimplemented namespace? Iterate the wire spec list so we catch
	// other_users prefixes even when no handle was opened for them.
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

// orderedHandles returns the implemented namespace handles in stable
// order (personal first, then by prefix length ascending). Drives
// cross-namespace LIST traversal — clients expect a stable order so
// reused connections see consistent listings.
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
