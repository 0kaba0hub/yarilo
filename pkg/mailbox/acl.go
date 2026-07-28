package mailbox

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// RFC 4314 §2.1 single-letter ACL rights. The 11 codes below are
// the canonical set used in this package. Obsolete codes 'c' / 'd'
// are accepted only on input by ParseRights and immediately expanded
// — they never appear in canonical output.
//
// Letter map for the canonical ACL right codes.
const (
	RightLookup        = 'l' // visible to LIST/LSUB/SELECT result
	RightRead          = 'r' // SELECT/EXAMINE/FETCH; \Seen excluded
	RightWriteSeen     = 's' // STORE of \Seen
	RightWrite         = 'w' // STORE of other flags except \Seen, \Deleted
	RightInsert        = 'i' // APPEND, COPY destination (private namespaces)
	RightPost          = 'p' // delivery via MTA (public / shared namespaces)
	RightCreate        = 'k' // CREATE / RENAME of children — was obsolete 'c'
	RightDeleteMailbox = 'x' // DELETE / RENAME of the mailbox itself
	RightDeleteMessage = 't' // STORE \Deleted on a message — was part of 'd'
	RightExpunge       = 'e' // EXPUNGE — was part of 'd'
	RightAdminister    = 'a' // SETACL / DELETEACL / GETACL / LISTRIGHTS
)

// allRights is the canonical sort order — every constructor in this
// file produces Rights in this order so == comparison and String()
// round-trip stay byte-stable.
var allRights = [...]rune{
	RightLookup,
	RightRead,
	RightWriteSeen,
	RightWrite,
	RightInsert,
	RightPost,
	RightCreate,
	RightDeleteMailbox,
	RightDeleteMessage,
	RightExpunge,
	RightAdminister,
}

var rightOrder = func() map[rune]int {
	m := make(map[rune]int, len(allRights))
	for i, r := range allRights {
		m[r] = i
	}
	return m
}()

// IsValidRight reports whether r is one of the 11 RFC 4314 rights.
// The obsolete 'c' / 'd' codes are NOT valid Right runes — they are
// only tolerated on input and expanded by ParseRights.
func IsValidRight(r rune) bool {
	_, ok := rightOrder[r]
	return ok
}

// Rights is a set of RFC 4314 right letters in canonical order.
// Always sorted, deduped, free of obsolete codes; equality is
// byte-equality on the underlying string.
type Rights string

// FullRights is the union of every RFC 4314 right.
const FullRights = Rights("lrswipkxtea")

// ParseRights normalises a wire/disk letter string into Rights:
//   - dedupes letters
//   - sorts to canonical order
//   - expands obsolete 'c' → 'k' and 'd' → 'te' (RFC 4314 §2.1.1)
//   - rejects any other character with a descriptive error
//
// Empty input returns the empty Rights set without error.
func ParseRights(s string) (Rights, error) {
	var have [128]bool
	for _, r := range s {
		switch {
		case r == 'c':
			have[RightCreate] = true
		case r == 'd':
			have[RightDeleteMessage] = true
			have[RightExpunge] = true
		case IsValidRight(r):
			have[r] = true
		default:
			return "", fmt.Errorf("mailbox/acl: invalid right %q", r)
		}
	}
	var b strings.Builder
	b.Grow(len(allRights))
	for _, r := range allRights {
		if have[r] {
			b.WriteRune(r)
		}
	}
	return Rights(b.String()), nil
}

// MustParseRights panics on error. Test helper.
func MustParseRights(s string) Rights {
	rs, err := ParseRights(s)
	if err != nil {
		panic(err)
	}
	return rs
}

// Has reports whether r is in the set.
func (rs Rights) Has(r rune) bool {
	return strings.ContainsRune(string(rs), r)
}

// HasAll reports whether every right in other is also in rs.
func (rs Rights) HasAll(other Rights) bool {
	for _, r := range other {
		if !rs.Has(r) {
			return false
		}
	}
	return true
}

// Add returns rs ∪ other in canonical form.
func (rs Rights) Add(other Rights) Rights {
	out, _ := ParseRights(string(rs) + string(other))
	return out
}

// Remove returns rs ∖ other in canonical form.
func (rs Rights) Remove(other Rights) Rights {
	var b strings.Builder
	b.Grow(len(rs))
	for _, r := range rs {
		if !other.Has(r) {
			b.WriteRune(r)
		}
	}
	return Rights(b.String())
}

// String returns the canonical letter string.
func (rs Rights) String() string { return string(rs) }

// IdentifierType enumerates the six identifier kinds RFC 4314 defines.
// Name is meaningful only for IDUser / IDGroup / IDGroupOverride.
// The zero value IDInvalid is never produced by ParseIdentifier —
// callers can use it as a sentinel.
type IdentifierType int

const (
	IDInvalid IdentifierType = iota
	IDAnyone
	IDAuthenticated
	IDOwner
	IDUser
	IDGroup
	IDGroupOverride
)

// Identifier is a parsed ACL identifier — the left side of a
// yarilo-acl line, without the optional '-' negativity marker.
type Identifier struct {
	Type IdentifierType
	Name string // user=/group=/group-override= only
}

// ParseIdentifier parses the wire/disk form into an Identifier.
// Rejects empty Name for user=/group=/group-override=. The '-'
// negativity marker is handled by ParseEntry, not here.
func ParseIdentifier(s string) (Identifier, error) {
	switch s {
	case "anyone":
		return Identifier{Type: IDAnyone}, nil
	case "authenticated":
		return Identifier{Type: IDAuthenticated}, nil
	case "owner":
		return Identifier{Type: IDOwner}, nil
	}
	for _, p := range []struct {
		prefix string
		typ    IdentifierType
	}{
		{"user=", IDUser},
		{"group-override=", IDGroupOverride},
		{"group=", IDGroup},
	} {
		if strings.HasPrefix(s, p.prefix) {
			name := strings.TrimPrefix(s, p.prefix)
			if name == "" {
				return Identifier{}, fmt.Errorf("mailbox/acl: empty %s identifier", strings.TrimSuffix(p.prefix, "="))
			}
			return Identifier{Type: p.typ, Name: name}, nil
		}
	}
	return Identifier{}, fmt.Errorf("mailbox/acl: unknown identifier %q", s)
}

// String returns the wire/disk form. Returns "" for IDInvalid so
// callers serialising an unvalidated Identifier produce a clearly
// malformed line rather than silent data.
func (id Identifier) String() string {
	switch id.Type {
	case IDAnyone:
		return "anyone"
	case IDAuthenticated:
		return "authenticated"
	case IDOwner:
		return "owner"
	case IDUser:
		return "user=" + id.Name
	case IDGroup:
		return "group=" + id.Name
	case IDGroupOverride:
		return "group-override=" + id.Name
	}
	return ""
}

// Entry is one parsed line of a yarilo-acl file. Negative=true
// entries strip Rights at evaluation time rather than granting them
// (RFC 4314 §3.5). Negatives are kept as separate entries — fold-in
// happens in the evaluator, not here.
type Entry struct {
	Identifier Identifier
	Rights     Rights
	Negative   bool
}

// ParseEntry parses one line of a yarilo-acl file:
//
//	[-]<identifier><WS><letters>
//
// Whitespace-only and '#'-prefixed lines return (Entry{}, false, nil)
// — the caller skips them. Trailing comments are not supported.
func ParseEntry(line string) (Entry, bool, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return Entry{}, false, nil
	}
	negative := false
	if strings.HasPrefix(trimmed, "-") {
		negative = true
		trimmed = trimmed[1:]
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return Entry{}, false, fmt.Errorf("mailbox/acl: empty entry")
	}
	if len(fields) > 2 {
		return Entry{}, false, fmt.Errorf("mailbox/acl: too many fields in %q", line)
	}
	id, err := ParseIdentifier(fields[0])
	if err != nil {
		return Entry{}, false, err
	}
	var rightsStr string
	if len(fields) == 2 {
		rightsStr = fields[1]
	}
	rights, err := ParseRights(rightsStr)
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{Identifier: id, Rights: rights, Negative: negative}, true, nil
}

// String returns the canonical wire/disk encoding of one entry
// without trailing newline. Empty Rights still serialises with a
// trailing space so `<id> ` round-trips — the format treats that as
// "grant nothing explicitly", which differs from "no entry".
func (e Entry) String() string {
	prefix := ""
	if e.Negative {
		prefix = "-"
	}
	return prefix + e.Identifier.String() + " " + e.Rights.String()
}

// ACL is the ordered set of entries from a yarilo-acl file. Order is
// preserved on read but is semantically insignificant — Negative=true
// entries subtract from any matching positive regardless of position.
// Sorted() returns a deterministic ordering for stable on-disk writes.
type ACL []Entry

// ParseACL reads a full yarilo-acl file body. Comment and blank
// lines are skipped; the first malformed entry aborts parsing with
// the 1-based line number annotated.
//
// Scanner buffer is bumped to 1 MiB so an oversized line surfaces as
// a scanner error rather than silent truncation.
func ParseACL(r io.Reader) (ACL, error) {
	var out ACL
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		entry, ok, err := ParseEntry(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("mailbox/acl: line %d: %w", line, err)
		}
		if !ok {
			continue
		}
		out = append(out, entry)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mailbox/acl: read: %w", err)
	}
	return out, nil
}

// ParseACLString is a convenience wrapper for tests and CLI input.
func ParseACLString(s string) (ACL, error) {
	return ParseACL(strings.NewReader(s))
}

// String returns the full yarilo-acl file encoding — one entry per
// line, each line LF-terminated. Empty ACL serialises to "" (no
// trailing newline) so an empty file is distinguishable from a
// single-empty-entry file.
func (acl ACL) String() string {
	if len(acl) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range acl {
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// Effective resolves the effective rights an authenticated user has
// on this mailbox's stored ACL.
//
// Semantics (RFC 4314 §3.5):
//   - isOwner == true: returns FullRights regardless of stored
//     entries (personal-namespace owner auto-grant).
//   - else: union of positive entries whose Identifier matches the
//     accessing user — anyone, authenticated, user=<user>,
//     group=<g> (when <g> is in groups), group-override=<g> — minus
//     negative entries matching the same identifiers.
//
// groups is the list of supplementary groups the user belongs to,
// sourced from the userdb `groups=` extra field. Pass nil or empty
// when no group membership is configured — group= entries have no
// effect.
//
// Identifier type priority (higher tier replaces lower when matched):
//  1. anyone / authenticated / group= → base tier
//  2. user= → replaces if any user= entry matches
//  3. group-override= → replaces if any group-override= matches
//
// A nil ACL with isOwner == false yields the empty rights set — no
// implicit grant for non-owners is the RFC 4314 default.
func (acl ACL) Effective(user string, groups []string, isOwner bool) Rights {
	pos, neg, _ := acl.effectiveMasks(user, groups, isOwner)
	return pos.Remove(neg)
}

// Identifier-type precedence tiers, lowest to highest. The most specific
// matching tier wins and REPLACES the rights of every lower tier (it does not
// merge with them); only within the group / group-override tiers do multiple
// matching entries merge. Order per RFC 4314 §3.5 / §6:
//
//	anyone < authenticated < group= < owner < user= < group-override=
const (
	tierAnyone = iota
	tierAuthenticated
	tierGroup
	tierOwner
	tierUser
	tierGroupOverride
	tierCount
)

// aclTier maps an identifier type to its precedence tier.
func aclTier(t IdentifierType) int {
	switch t {
	case IDAuthenticated:
		return tierAuthenticated
	case IDGroup:
		return tierGroup
	case IDOwner:
		return tierOwner
	case IDUser:
		return tierUser
	case IDGroupOverride:
		return tierGroupOverride
	default: // IDAnyone (and IDInvalid, unreachable for stored entries)
		return tierAnyone
	}
}

// effectiveMasks resolves the user's positive and negative right masks by
// applying every matching entry in ascending tier order:
//
//   - A positive entry at a new tier REPLACES the running positive mask;
//     within the group / group-override tiers positive entries ADD (merge).
//   - A negative entry only subtracts — it never resets the positive mask —
//     so a negative user= entry revokes rights without wiping a lower tier's
//     positive grant; its negative mask REPLACEs at a new tier, ADDs within.
//   - An owner with no explicit owner-tier entry gets full rights at the owner
//     tier (a matching user=/group-override= entry can still replace it).
//
// Effective is pos.Remove(neg); the masks are exposed so a global ACL can be
// merged with the right precedence (see EffectiveWithGlobal).
func (acl ACL) effectiveMasks(user string, groups []string, isOwner bool) (pos, neg Rights, matched bool) {
	groupSet := makeGroupSet(groups)

	type match struct {
		tier     int
		rights   Rights
		negative bool
	}
	var matches []match
	for _, e := range acl {
		var isMatch bool
		switch e.Identifier.Type {
		case IDAnyone, IDAuthenticated:
			isMatch = true
		case IDOwner:
			isMatch = isOwner
		case IDUser:
			isMatch = e.Identifier.Name == user
		case IDGroup, IDGroupOverride:
			isMatch = groupSet[e.Identifier.Name]
		}
		if !isMatch {
			continue
		}
		matches = append(matches, match{aclTier(e.Identifier.Type), e.Rights, e.Negative})
	}
	// Owner default: full positive rights at the owner tier when no explicit
	// owner-tier entry exists.
	if isOwner {
		hasOwnerTier := false
		for _, m := range matches {
			if m.tier == tierOwner {
				hasOwnerTier = true
				break
			}
		}
		if !hasOwnerTier {
			matches = append(matches, match{tierOwner, FullRights, false})
		}
	}
	if len(matches) == 0 {
		return "", "", false
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].tier < matches[j].tier })

	var myPos, myNeg Rights
	prevTier := -1
	for _, m := range matches {
		boundary := m.tier != prevTier
		if m.negative {
			if boundary {
				myNeg = Rights("").Add(m.rights) // REPLACE (canonicalised)
			} else {
				myNeg = myNeg.Add(m.rights)
			}
		} else {
			if boundary {
				myPos = Rights("").Add(m.rights) // REPLACE (canonicalised)
			} else {
				myPos = myPos.Add(m.rights)
			}
		}
		prevTier = m.tier
	}
	return myPos, myNeg, true
}

// EffectiveWithGlobal resolves rights from a local ACL and a global ACL with
// the global taking precedence: global positives add on top of the local
// result and global negatives revoke even locally-granted rights. When any
// global entry matches the user, local negative rights are reset so they
// cannot undermine a global grant. The owner default (full rights) flows
// through the local masks, and a matching global entry can still override it.
func EffectiveWithGlobal(local, global ACL, user string, groups []string, isOwner bool) Rights {
	lpos, lneg, _ := local.effectiveMasks(user, groups, isOwner)
	gpos, gneg, gmatched := global.effectiveMasks(user, groups, false)
	if gmatched {
		return lpos.Add(gpos).Remove(gneg)
	}
	return lpos.Remove(lneg)
}

// makeGroupSet builds a fast-lookup set from a groups slice.
func makeGroupSet(groups []string) map[string]bool {
	if len(groups) == 0 {
		return nil
	}
	s := make(map[string]bool, len(groups))
	for _, g := range groups {
		s[g] = true
	}
	return s
}

// Sorted returns a copy with entries in deterministic order:
// identifiers grouped by Type (anyone, authenticated, owner, user=,
// group=, group-override=) and then alphabetically by Name; within
// the same identifier, positive entries precede negative.
func (acl ACL) Sorted() ACL {
	out := make(ACL, len(acl))
	copy(out, acl)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Identifier.Type != out[j].Identifier.Type {
			return out[i].Identifier.Type < out[j].Identifier.Type
		}
		if out[i].Identifier.Name != out[j].Identifier.Name {
			return out[i].Identifier.Name < out[j].Identifier.Name
		}
		return !out[i].Negative && out[j].Negative
	})
	return out
}
