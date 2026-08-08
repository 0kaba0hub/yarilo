package mailbox

import (
	"fmt"
	"strings"
)

// OwnerVar is the owner variable a namespace prefix carries to become
// owner-templated: the segment filling its slot names the owner of the instance
// (docs/OWNER_SHARED_NS.md 3.1). v1 supports %u (the full username); %n / %d
// split-slot forms are a documented follow-up.
const OwnerVar = "%u"

// PrefixIsOwnerTemplated reports whether a namespace prefix carries the owner
// variable. One definition, shared by config validation and the IMAP resolver,
// so the two cannot disagree about what "owner-templated" means.
func PrefixIsOwnerTemplated(prefix string) bool {
	return strings.Contains(prefix, OwnerVar)
}

// ExtractOwner parses a wire mailbox name under an owner-templated prefix into
// the owner it names and the folder relative to that owner's store. sep is the
// namespace separator (0 defaults to '/').
//
//	prefix "user/%u/", "user/alice/Sent" -> "alice", "Sent"
//	                   "user/alice"       -> "alice", "INBOX"
//
// ok is false when the name is not under the prefix, when the owner segment is
// malformed (empty, "." or ".."), or when the post-variable literal is anything
// but the separator (the v1 limitation). A false result must resolve to no
// owner, never to the requesting user -- the honesty isOwner depends on.
func ExtractOwner(prefix string, sep byte, name string) (owner, rel string, ok bool) {
	if !PrefixIsOwnerTemplated(prefix) {
		return "", "", false
	}
	before, after, _ := strings.Cut(prefix, OwnerVar)
	if !strings.HasPrefix(name, before) {
		return "", "", false
	}
	s := "/"
	if sep != 0 {
		s = string(sep)
	}
	if after != "" && after != s {
		return "", "", false
	}
	seg, tail, hasSep := strings.Cut(name[len(before):], s)
	if !validOwnerSegment(seg) {
		return "", "", false
	}
	if !hasSep || tail == "" {
		return seg, "INBOX", true
	}
	return seg, tail, true
}

// validOwnerSegment rejects a segment that cannot be a username: empty, or the
// filesystem-relative "." / "..". The userdb lookup fails closed for the rest.
func validOwnerSegment(seg string) bool {
	switch seg {
	case "", ".", "..":
		return false
	}
	return true
}

// StampOwnerLocation builds the storage identity for the owner of an
// owner-templated namespace instance from the owner's ALREADY-RESOLVED userdb
// UserInfo. The lookup that produces owner stays with each caller (the IMAP
// on-demand cache, the admin AuthClient) -- one producer here, not a lookup
// parameter each caller fills differently.
//
// Precedence (docs/OWNER_SHARED_NS.md 3.3): the owner's userdb decides the
// driver and the root (Home/MailPath); the namespace location fills only fields
// the userdb left empty. Overwriting the root with the template would point a
// per-user driver at a path it does not match -- the parallel-tree bug. base,
// when non-nil, supplies the deployment-wide storage-name form.
func StampOwnerLocation(owner, base *UserInfo, location string, sep byte) (*UserInfo, error) {
	if owner == nil {
		return nil, fmt.Errorf("mailbox: nil owner UserInfo")
	}
	ui := *owner // copy: callers may hold owner in a cache
	out := &ui

	loc, ok, err := ParseLocation(location, out)
	if err != nil {
		return nil, fmt.Errorf("mailbox: owner location %q: %w", location, err)
	}
	if !ok || loc.Path == "" {
		return nil, fmt.Errorf("mailbox: owner location %q resolved to no path", location)
	}
	fillIfEmpty(&out.MailPath, loc.Path)
	fillIfEmpty(&out.Home, loc.Path)
	out.Separator = string(sep)
	fillIfEmpty(&out.IndexDir, loc.IndexDir)
	fillIfEmpty(&out.VolatileDir, loc.VolatileDir)
	fillIfEmpty(&out.ControlDir, loc.ControlDir)
	fillIfEmpty(&out.AltDir, loc.AltDir)
	if base != nil {
		out.StorageEscapeChar = base.StorageEscapeChar
		out.SkipNFCNormalize = base.SkipNFCNormalize
	}
	return out, nil
}

func fillIfEmpty(dst *string, src string) {
	if *dst == "" {
		*dst = src
	}
}

// NamespaceFileSlug builds the per-namespace component of an on-disk filename
// ("subscriptions-<slug>") from a namespace prefix. It is always ONE path
// segment: the prefix is a client-visible name, so it carries the hierarchy
// separator and may carry per-user variables, and using it raw made
// "subscriptions-user/%u" a directory named subscriptions-user holding a file
// %u -- a path where a filename was intended, spelled from an unexpanded
// template (#1159).
//
// Everything from the first variable onwards is dropped: those parts vary per
// user, while this names a file inside one store, so they can only mislead
// (the file describes no owner). Remaining separators become '-'. A prefix that
// reduces to nothing falls back to the namespace type, as an empty prefix does.
//
// A prefix with no separator and no variable is unchanged, so the common
// "Public/" and "Shared/" keep the names already on disk.
func NamespaceFileSlug(prefix, separator, nsType string) string {
	name := prefix
	if i := strings.IndexByte(name, '%'); i >= 0 {
		name = name[:i]
	}
	sep := separator
	if sep == "" {
		sep = "/"
	}
	name = strings.Trim(name, sep)
	name = strings.ReplaceAll(name, sep, "-")
	// Path separators are never part of one filename, whatever the namespace
	// declared as its hierarchy separator.
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, `\`, "-")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return strings.ToLower(strings.TrimSpace(nsType))
	}
	return name
}

// NamespaceSubsFile is the filename holding a namespace's subscription state,
// and the one producer of it. The personal namespace keeps the bare
// "subscriptions" so an upgrade does not orphan state already on disk; every
// other namespace gets a "subscriptions-<slug>" sibling.
//
// One producer because the rule had been written out at each site that needed
// it -- and the one that did not know about the personal case rejected a valid
// config, reporting a collision between files that are not the same file
// (#1159). A caller comparing or building these names must use this, not
// "subscriptions-" + NamespaceFileSlug.
func NamespaceSubsFile(prefix, separator, nsType string) string {
	if strings.EqualFold(strings.TrimSpace(nsType), "personal") {
		return "subscriptions"
	}
	return "subscriptions-" + NamespaceFileSlug(prefix, separator, nsType)
}

// NamespaceKeepsSubscriptions decides whether a namespace stores subscriptions
// for the mailboxes under it, or delegates them to the namespace that does --
// the subscriber's own, normally the personal one. explicit is the operator's
// setting, nil when unset.
//
//   - personal: always keeps them, so a deployment always has one namespace that
//     can hold a subscription.
//   - owner-templated: never. Its storage resolves per owner at runtime, so "the
//     namespace's own subscription file" names no owner at all -- a
//     configuration without a meaning rather than a dangerous one. Asking for
//     one fails at startup (pkg/config) instead of picking an owner silently.
//   - fixed shared/public: delegates by default, keeps them only on explicit
//     subscriptions: true. The reference implementation defaults to true and
//     leans on filesystem permissions and a per-user control path as the
//     barrier; our processes share one uid on an RWX volume, so that barrier
//     does not exist -- a deliberate divergence. A shared subscription file (a
//     site-wide list) stays available as an opt-in.
//
// One rule, one implementation: config resolves it for the wire and IMAP for the
// session, and both ask this.
func NamespaceKeepsSubscriptions(nsType, prefix string, explicit *bool) bool {
	if strings.EqualFold(strings.TrimSpace(nsType), "personal") {
		return true
	}
	if PrefixIsOwnerTemplated(prefix) {
		return false
	}
	if explicit == nil {
		return false
	}
	return *explicit
}
