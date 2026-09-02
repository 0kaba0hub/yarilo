package mailbox

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
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

// maxIdentifierLen matches the reference's identifier bound; a longer one is
// refused before it can reach a file.
const maxIdentifierLen = 1024

// ValidIdentifier refuses what the line-oriented format cannot carry: an
// over-long identifier, invalid UTF-8, or a control character (a newline in a
// name would end the entry early and start a forged one).
func ValidIdentifier(s string) error {
	if len(s) > maxIdentifierLen {
		return fmt.Errorf("mailbox/acl: identifier longer than %d bytes", maxIdentifierLen)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("mailbox/acl: identifier is not valid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("mailbox/acl: identifier contains a control character")
		}
	}
	return nil
}

// ParseIdentifier parses the wire/disk form into an Identifier.
// Rejects empty Name for user=/group=/group-override= and anything
// the on-disk format cannot represent (ValidIdentifier). The '-'
// negativity marker is handled by ParseEntry, not here.
func ParseIdentifier(s string) (Identifier, error) {
	if err := ValidIdentifier(s); err != nil {
		return Identifier{}, err
	}
	switch s {
	case "anyone", "anonymous": // anonymous is the reference's spelling
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
	if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
		return Entry{}, false, nil
	}
	s := strings.TrimRight(strings.TrimLeft(line, " \t"), "\r")

	// The reference's format: an identifier containing a space is written as
	// a backslash-escaped quoted string, the '-' of a negative entry INSIDE
	// the quotes; an unquoted identifier carries no space and splits at the
	// FIRST one. Rights are the single remaining field.
	var idStr, rest string
	if strings.HasPrefix(s, `"`) {
		var err error
		idStr, rest, err = unquoteACL(s)
		if err != nil {
			return Entry{}, false, err
		}
	} else {
		idStr, rest = s, ""
		if i := strings.IndexAny(s, " \t"); i >= 0 {
			idStr, rest = s[:i], s[i+1:]
		}
	}
	negative := false
	if strings.HasPrefix(idStr, "-") {
		negative = true
		idStr = idStr[1:]
	}
	if idStr == "" {
		return Entry{}, false, fmt.Errorf("mailbox/acl: empty entry")
	}
	rightsStr := strings.Trim(rest, " \t")
	if strings.ContainsAny(rightsStr, " \t") {
		return Entry{}, false, fmt.Errorf("mailbox/acl: too many fields in %q", line)
	}
	id, err := ParseIdentifier(idStr)
	if err != nil {
		return Entry{}, false, err
	}
	rights, err := ParseRights(rightsStr)
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{Identifier: id, Rights: rights, Negative: negative}, true, nil
}

// unquoteACL reads a leading quoted identifier: backslash escapes the next
// byte, the closing quote must be followed by a separator or end the line.
// Returns the unescaped content and what follows the separator.
func unquoteACL(s string) (content, rest string, err error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		switch c := s[i]; c {
		case '\\':
			if i+1 >= len(s) {
				return "", "", fmt.Errorf("mailbox/acl: unterminated escape in quoted identifier")
			}
			b.WriteByte(s[i+1])
			i += 2
		case '"':
			i++
			if i == len(s) {
				return b.String(), "", nil
			}
			if s[i] != ' ' && s[i] != '\t' {
				return "", "", fmt.Errorf("mailbox/acl: garbage after quoted identifier")
			}
			return b.String(), s[i+1:], nil
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", "", fmt.Errorf("mailbox/acl: unterminated quoted identifier")
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
	id := prefix + e.Identifier.String()
	// The reference's quoting: an identifier a plain split would misread is
	// written as a backslash-escaped quoted string, sign inside the quotes.
	if strings.ContainsAny(id, " \t\"\\") {
		id = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(id) + `"`
	}
	return id + " " + e.Rights.String()
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
	return out.Collapse(), nil
}

// Collapse reduces the ACL to one entry per (identifier, sign): the last line
// for an identifier is the one that counts, in the position the first one held.
//
// Last wins rather than union, because a line is a statement about an
// identifier and the later statement is the current one -- the same thing a
// write means. Union would preserve every grant ever appended, which is the
// behaviour that made reduction impossible: "lrskxa" followed by "lr" would
// still resolve to "lrskxa". It also errs towards fewer rights when a file is
// ambiguous, which is the direction to err in.
//
// Duplicates were legal on disk and unioned at evaluation time, which made a
// write path that appended instead of replacing look like it worked: granting
// "lr" and then "sk" resolved to "lrsk", and an attempt to reduce "lrskxa" to
// "lr" resolved to "lrskxa" -- so an ACL could only ever widen, and the only
// trace was a second line in a file nobody opens (#1114).
//
// Collapsing at parse rather than at write means it holds for files written by
// any past version, by hand, or by a tool we do not own, instead of only for
// the writes we control.
//
// The two signs stay separate entries -- the file format has no single line
// carrying both -- but there is now at most one of each per identifier, so the
// negative mask for an identifier is decided by one entry rather than by how
// many lines happen to mention it and in what order.
func (acl ACL) Collapse() ACL {
	if len(acl) < 2 {
		return acl
	}
	type key struct {
		id       Identifier
		negative bool
	}
	at := make(map[key]int, len(acl))
	out := make(ACL, 0, len(acl))
	for _, e := range acl {
		k := key{e.Identifier, e.Negative}
		if i, seen := at[k]; seen {
			out[i].Rights = e.Rights
			continue
		}
		at[k] = len(out)
		out = append(out, e)
	}
	return out
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

// IdentifierNamesOwner reports whether id addresses the owner: their user=
// identity (either sign) or the owner keyword. Such an entry is inert under the
// strong grant. The keyword is inert for everyone, so it counts even when
// owner == "".
func IdentifierNamesOwner(id Identifier, owner string) bool {
	if id.Type == IDOwner {
		return true
	}
	return owner != "" && id.Type == IDUser && id.Name == owner
}

// OwnerImmutableReason explains a refused owner-naming ACL write. The keyword
// text does not presuppose an owner (it is refused in an ownerless namespace).
func OwnerImmutableReason(id Identifier) string {
	if id.Type == IDOwner {
		return "the owner keyword cannot be set: it matches no identity (the mailbox owner always holds full rights)"
	}
	return "the owner's rights cannot be changed through ACL; freeze a mailbox by another mechanism"
}

// Effective resolves the rights user has on this stored ACL (RFC 4314 §3.5).
// isOwner true returns FullRights regardless of entries (strong grant, §7.6);
// otherwise positive matches by tier precedence minus matching negatives.
func (acl ACL) Effective(user string, groups []string, isOwner bool) Rights {
	if isOwner {
		return FullRights
	}
	pos, neg, _ := acl.effectiveMasks(user, groups)
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

// effectiveMasks resolves the user's positive and negative right masks by applying
// every matching entry in ascending tier order: a mask REPLACES at a new tier and
// ADDs (merges) within a tier, separately for positives and negatives. It resolves
// non-owners only; the owner is decided at the boundary (Effective /
// EffectiveWithGlobal short-circuit to FullRights), so an owner-tier stored entry
// never applies here. The masks are returned separately so a global ACL can be
// merged at the right precedence (see EffectiveWithGlobal); Effective itself is
// pos.Remove(neg).
func (acl ACL) effectiveMasks(user string, groups []string) (pos, neg Rights, matched bool) {
	pos, neg, matched, _ = acl.effectiveMasksSigned(user, groups)
	return pos, neg, matched
}

// effectiveMasksSigned is effectiveMasks plus whether a positive entry matched.
// A caller layering one ACL over another needs to know whether the upper layer
// spoke about the positive mask: a global entry that only revokes must not
// blank the positive rights the local ACL granted, while one that grants must
// replace them (#1117).
func (acl ACL) effectiveMasksSigned(user string, groups []string) (pos, neg Rights, matched, posMatched bool) {
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
			// Owner is resolved at the boundary; this runs for non-owners only,
			// and an owner-tier entry never applies to a non-owner.
			isMatch = false
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
	if len(matches) == 0 {
		return "", "", false, false
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].tier < matches[j].tier })

	// Each mask keeps its own tier boundary. A single prevTier shared by the
	// two streams let whichever sign came first at a tier eat the boundary, so
	// the second merged with the tier below instead of replacing it -- and
	// which sign came first was decided by Sorted()'s file order, so our own
	// files took the wrong branch (#1117).
	var myPos, myNeg Rights
	prevPosTier, prevNegTier := -1, -1
	for _, m := range matches {
		if m.negative {
			if m.tier != prevNegTier {
				myNeg = Rights("").Add(m.rights) // REPLACE (canonicalised)
			} else {
				myNeg = myNeg.Add(m.rights)
			}
			prevNegTier = m.tier
		} else {
			if m.tier != prevPosTier {
				myPos = Rights("").Add(m.rights) // REPLACE (canonicalised)
			} else {
				myPos = myPos.Add(m.rights)
			}
			prevPosTier = m.tier
		}
	}
	return myPos, myNeg, true, prevPosTier >= 0
}

// EffectiveWithGlobal resolves rights from a local ACL and a global ACL, with
// the global ACL forming a tier ladder above the local one: a matching global
// entry REPLACES the local result rather than adding to it, and the local
// negative mask is reset at the first global match so local negatives cannot
// undermine a global grant, which is the 2.4 behaviour.
//
// It used to be `lpos.Add(gpos).Remove(gneg)`, which failed open twice over:
// global positives added to local ones, and every local negative was discarded
// whenever any global entry matched. So a local "-user=alice a" plus an
// unrelated global "anyone l" re-granted alice the administer right (#1117).
func EffectiveWithGlobal(local, global ACL, user string, groups []string, isOwner bool) Rights {
	// Owner beats everything, global included: #1118 makes global replace local,
	// so without this a global negative would strip the owner (§7.6).
	if isOwner {
		return FullRights
	}
	lpos, lneg, _ := local.effectiveMasks(user, groups)
	gpos, gneg, gmatched, gposMatched := global.effectiveMasksSigned(user, groups)
	if !gmatched {
		return lpos.Remove(lneg)
	}
	// First global match resets the local negatives, so they cannot undermine
	// a global grant. Then each mask the globals spoke about replaces the local
	// one; a global that only revokes leaves the local positives standing.
	pos := lpos
	if gposMatched {
		pos = gpos
	}
	return pos.Remove(gneg)
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
