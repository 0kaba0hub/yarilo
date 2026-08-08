package imap

import (
	"context"
	"fmt"
	"log/slog"
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
	// synthetic UserInfo whose Home is the namespace root -- so userInfo.Username
	// is the *session* user, not an owner, which is why comparing the two once
	// made every peer look like the owner (#1107).
	userInfo *mailbox.UserInfo
	// owner is the person who owns this instance of the namespace, or "" when
	// none does. A personal namespace is owned by the session user; a fixed
	// shared or public namespace is owned by nobody -- there is no principal who
	// holds rights implicitly, which is what makes a bootstrap grant necessary
	// (see docs/OWNER_SHARED_NS.md 7.2). An owner-templated namespace (B1) will
	// carry the resolved owner here. isOwner compares this against the session
	// user, so the definition is by person, not by namespace type.
	owner string
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
			h, err := s.openHandle(spec, "personal", personalUI, owner, mailbox.NamespaceSubsFile(spec.Prefix, string(spec.Separator), string(spec.Type)))
			if err != nil {
				return nil, nil, fmt.Errorf("imap: open personal namespace: %w", err)
			}
			out[spec.Prefix] = h
			if primary == nil {
				primary = h
			}
		case NamespaceShared, NamespaceOther: //nolint:exhaustive
			if isOwnerTemplated(spec) {
				// Owner-templated: the owner is not known at login, so no
				// handle is built here. dispatch resolves it on demand, per
				// referenced owner, and caches it on the session (§3.4).
				// Expanding the location with a nil owner now would produce a
				// degenerate path shared by everyone -- the parallel tree this
				// design prevents -- so it is deliberately not attempted.
				continue
			}
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
			subsFile := mailbox.NamespaceSubsFile(spec.Prefix, string(spec.Separator), string(spec.Type))
			// MailPath as well as Home, plus driver and modifiers, so the
			// mailbox backend, fileindex and ACL store resolve to one root;
			// otherwise the ACL store (falls back to Home) and a maildir
			// backend (falls back to Home/Maildir) disagree. Shared with the
			// admin API, which built the same structure from two fields and
			// therefore disagreed with this one (#1109).
			ui, err := mailbox.NamespaceUserInfo(personalUI, loc, string(spec.Separator))
			if err != nil {
				return nil, nil, fmt.Errorf("imap: %s namespace: %w", spec.Type, err)
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
		h, err := s.openHandle(fallback, "personal", personalUI, owner, mailbox.NamespaceSubsFile(fallback.Prefix, string(fallback.Separator), string(fallback.Type)))
		if err != nil {
			return nil, nil, fmt.Errorf("imap: open fallback personal namespace: %w", err)
		}
		out[""] = h
		primary = h
	}
	return out, primary, nil
}

// openHandle wires one namespace's box + idx + subs. The mailbox backend is
// selected by the resolved user's driver (see mailboxBackendFor).
func (s *session) openHandle(spec NamespaceSpec, name string, ui *mailbox.UserInfo, owner, subsFile string) (*nsHandle, error) {
	ui.Separator = string(spec.Separator)
	mb := s.mailboxBackendFor(spec, ui)
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
	aclStore := acl.New(ui.Home, ui.MailPath, ui.Driver, ui.Separator, ui.StorageEscapeChar, ui.Username, owner, acl.Policy{
		DefaultsFromInbox: defaultsFromInbox,
		GlobalsOnly:       s.srv.opts.ACLGlobalsOnly,
		CacheTTL:          s.srv.opts.ACLCacheTTL,
		Global:            s.srv.opts.ACLGlobal,
	}, s.srv.opts.Locker)
	// The namespace-instance owner. A personal namespace is owned by the user
	// it was opened for; a fixed shared/public one by nobody. Owner-templated
	// namespaces (B1) set this to the resolved owner, and isOwner then works for
	// them through the same definition rather than a second one.
	nsOwner := ""
	if spec.Type == NamespacePersonal {
		nsOwner = ui.Username
	}
	return &nsHandle{
		name:     name,
		spec:     spec,
		box:      box,
		idx:      idx,
		subs:     store,
		acl:      aclStore,
		userInfo: ui,
		owner:    nsOwner,
	}, nil
}

// mailboxBackendFor returns the MailboxBackend for a namespace, selected by the
// resolved user's driver -- ui is the owner's userdb identity for an
// owner-templated namespace, the session user's for the personal one, so a
// per-user driver (mdbox/maildir/sdbox) opens the right store. Selecting by
// spec.Prefix instead left the driver keyed on a value one prefix serves for
// every owner, so an owner-templated namespace opened the global backend on a
// foreign-driver root -- the phantom store (#1144). Same selection the admin
// path already uses (backendapi/userctx.go).
//
// A NamespaceMailboxes override still wins where set, but on an owner-templated
// namespace it applies to every owner alike (one backend, one prefix) -- correct
// only when all owners share a format.
func (s *session) mailboxBackendFor(spec NamespaceSpec, ui *mailbox.UserInfo) mailbox.MailboxBackend {
	if override, ok := s.srv.opts.NamespaceMailboxes[spec.Prefix]; ok && override != nil {
		return override
	}
	if ui != nil && ui.Driver != "" {
		return mailbox.SelectPersonalBackend(s.srv.opts.Mailbox, s.srv.opts.MailboxByDriver, ui.Driver)
	}
	return s.srv.opts.Mailbox
}

// nsSlug is an in-memory identifier for a namespace (handle name, log field).
// It is NOT a filename: it keeps the prefix as the client sees it, separator and
// template variable intact. Anything that names a file must go through
// mailbox.NamespaceSubsFile / NamespaceFileSlug, which produce one path segment
// (#1159).
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
	// Normalise the wire name to its final form here, at the one point that
	// produces the rel every tree is addressed by, so no later use of rel can
	// see a different spelling than the mail directory holds (#1113). INBOX is
	// canonical ASCII and returns above.
	name = s.normaliseName(name)

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

	// Owner-templated namespaces: resolve the owner from the name and open its
	// handle on demand (§3.4). A malformed owner (traversal, empty) makes
	// extractOwner return ok=false, so it falls through to the literal match and
	// then to the personal namespace -- resolving to nobody, never the caller,
	// which is what keeps isOwner honest (#544/B1).
	for _, spec := range specs {
		if !isOwnerTemplated(spec) {
			continue
		}
		owner, rel, ok := extractOwner(spec, name)
		if !ok {
			continue
		}
		// An owner that does not resolve and an owner whose space the caller
		// cannot see must be indistinguishable. Here the probed segment IS a
		// username, so a NOPERM/NONEXISTENT split over user/<name>/ is a
		// directory of the deployment's accounts to anyone who can log in
		// (#1138). One error value serves both, so the two answers cannot drift
		// apart later.
		hidden := &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
		h, err := s.ownerHandle(spec, owner)
		if err != nil {
			return nil, "", hidden
		}
		// Decided here rather than per verb: CREATE deliberately does not hide
		// (#1068, and rightly so where the namespace is public), and SUBSCRIBE
		// checks no rights at all -- so both leaked. At the resolve layer every
		// verb inherits the answer, including ones added later.
		if !s.ownerSpaceVisible(h, rel) {
			return nil, "", hidden
		}
		return h, rel, nil
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

// normaliseName applies the deployment's NFC policy to a wire folder name. It
// is the single owner of the transformation; every other tree takes the name
// as given (mailbox.NormalizeName).
func (s *session) normaliseName(name string) string {
	skip := s.userInfo != nil && s.userInfo.SkipNFCNormalize
	return mailbox.NormalizeName(name, skip)
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

// maxOwnerHandles bounds the per-session owner-handle cache so a session that
// walks many owners does not grow unbounded. When full, the oldest-inserted
// handle is evicted and closed. Not strict LRU -- insertion order, not access
// order -- which is enough to cap memory; a hot owner re-resolved after eviction
// costs one userdb lookup, logged at debug.
const maxOwnerHandles = 64

// ownerSpaceVisible reports whether the caller may learn that this owner's space
// exists. The owner always may; a peer may once they hold any right on the name
// (they already know it is there, so the precise refusal is not a disclosure).
// A peer holding nothing is told exactly what an unknown owner is told.
//
// An ACL that cannot be read hides too: the failure is only reachable for an
// owner that resolved, so surfacing it would answer the very question the hiding
// exists to refuse. It is logged instead of reported.
func (s *session) ownerSpaceVisible(h *nsHandle, rel string) bool {
	if !s.aclEnforced(h) || s.isOwner(h) {
		return true
	}
	rights, err := s.effectiveRights(h, rel)
	if err != nil {
		slog.Error("imap: owner-templated ACL read failed; hiding the space",
			"user", s.userInfo.Username, "owner", h.owner, "folder", rel, "err", err)
		return false
	}
	return rights != ""
}

// deploymentBase carries only the deployment-wide storage-name form
// (StorageEscapeChar, SkipNFCNormalize) a namespace producer stamps, so a
// mailbox is spelled the same on disk whoever opens it (#1078, #1092). It comes
// from the resolver's deployment defaults, not the session user: passing the
// accessor's identity was the same "admin right, IMAP wrong" divergence as the
// driver (#1144) -- once userdb answers these per user, an owner's tree would be
// spelled by whoever opened it. Only the two fields StampOwnerLocation reads are
// set, so a later reader cannot pick up a Home resolved for the empty name.
func (s *session) deploymentBase() *mailbox.UserInfo {
	r := s.srv.opts.Resolver
	if r == nil {
		return nil
	}
	full := r.UserInfo("", "")
	return &mailbox.UserInfo{
		StorageEscapeChar: full.StorageEscapeChar,
		SkipNFCNormalize:  full.SkipNFCNormalize,
	}
}

// ownerHandle returns the handle for one owner of an owner-templated namespace,
// building it on first reference and caching it for the session. The owner is
// resolved through the userdb (resolveOwnerUserInfo), so the handle carries the
// owner's per-user driver and root; h.owner is set to the owner, which is what
// makes isOwner true for the owner's own session and false for a peer (#1130).
func (s *session) ownerHandle(spec NamespaceSpec, owner string) (*nsHandle, error) {
	key := spec.Prefix + "\x00" + owner
	if s.ownerHandles != nil {
		if h, ok := s.ownerHandles[key]; ok {
			return h, nil
		}
	}
	ownerUI, err := resolveOwnerUserInfo(context.Background(), s.srv.opts.UserdbLookup, s.deploymentBase(), spec, owner)
	if err != nil {
		return nil, err
	}
	subsFile := mailbox.NamespaceSubsFile(spec.Prefix, string(spec.Separator), string(spec.Type))
	h, err := s.openHandle(spec, nsSlug(spec), ownerUI, s.owner(ownerUI.Username), subsFile)
	if err != nil {
		return nil, err
	}
	// The person who owns this instance -- the resolved owner, not the session
	// user. isOwner compares this against the session user (#1130).
	h.owner = owner
	h.location = ownerUI.MailPath

	if s.ownerHandles == nil {
		s.ownerHandles = make(map[string]*nsHandle)
	}
	if len(s.ownerHandles) >= maxOwnerHandles {
		s.evictOneOwnerHandle()
	}
	s.ownerHandles[key] = h
	return h, nil
}

// evictOneOwnerHandle closes and removes one cached owner handle to stay under
// the bound. Arbitrary victim (map order); the bound is a memory cap, not a
// fairness policy.
func (s *session) evictOneOwnerHandle() {
	for k, h := range s.ownerHandles {
		closeHandle(h)
		delete(s.ownerHandles, k)
		return
	}
}

// closeHandles releases per-namespace resources at session teardown.
func (s *session) closeHandles() {
	for _, h := range s.namespaces {
		closeHandle(h)
	}
	for _, h := range s.ownerHandles {
		closeHandle(h)
	}
}

func closeHandle(h *nsHandle) {
	if h == nil {
		return
	}
	if h.box != nil {
		h.box.Close() //nolint:errcheck
	}
	if h.idx != nil {
		h.idx.Close() //nolint:errcheck
	}
}

// owner builds the yarilo-locks owner string for diagnostics.
func (s *session) owner(username string) string {
	return fmt.Sprintf("yarilo-imap/%d/%s", os.Getpid(), username)
}
