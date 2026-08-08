package mailbox

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestParseRights(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Rights
		wantErr bool
	}{
		{name: "empty", in: "", want: ""},
		{name: "all canonical", in: "lrswipkxtea", want: FullRights},
		{name: "scrambled order canonicalises", in: "axelpriwskt", want: FullRights},
		{name: "dedupe", in: "rrrrl", want: "lr"},
		{name: "obsolete c → k", in: "c", want: "k"},
		{name: "obsolete d → te", in: "d", want: "te"},
		{name: "obsolete c+d coexist with canonical letters", in: "lrscdk", want: "lrskte"},
		{name: "invalid uppercase", in: "L", wantErr: true},
		{name: "invalid symbol", in: "lrs!", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRights(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRights(%q): want error, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRights(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseRights(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRights_HasHasAll(t *testing.T) {
	r := MustParseRights("lra")
	if !r.Has('l') {
		t.Error("Has(l) should be true")
	}
	if r.Has('w') {
		t.Error("Has(w) should be false")
	}
	if !r.HasAll(MustParseRights("lr")) {
		t.Error("HasAll(lr) should be true")
	}
	if r.HasAll(MustParseRights("lrw")) {
		t.Error("HasAll(lrw) should be false")
	}
	if !r.HasAll("") {
		t.Error("HasAll(empty) should always be true")
	}
}

func TestRights_AddRemove(t *testing.T) {
	tests := []struct {
		name           string
		base, op, want Rights
		isAdd          bool
	}{
		{name: "add disjoint", base: "lr", op: "wa", want: "lrwa", isAdd: true},
		{name: "add overlapping", base: "lr", op: "rs", want: "lrs", isAdd: true},
		{name: "add expands obsolete", base: "l", op: "c", want: "lk", isAdd: true},
		{name: "add to empty", base: "", op: "lrwa", want: "lrwa", isAdd: true},
		{name: "remove subset", base: FullRights, op: "lra", want: "swipkxte"},
		{name: "remove disjoint is noop", base: "lr", op: "wa", want: "lr"},
		{name: "remove everything", base: "lrwa", op: FullRights, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Rights
			if tc.isAdd {
				got = tc.base.Add(tc.op)
			} else {
				got = tc.base.Remove(tc.op)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Identifier
		wantErr bool
	}{
		{name: "anyone", in: "anyone", want: Identifier{Type: IDAnyone}},
		{name: "authenticated", in: "authenticated", want: Identifier{Type: IDAuthenticated}},
		{name: "owner", in: "owner", want: Identifier{Type: IDOwner}},
		{name: "user", in: "user=alice", want: Identifier{Type: IDUser, Name: "alice"}},
		{name: "group", in: "group=staff", want: Identifier{Type: IDGroup, Name: "staff"}},
		{name: "group-override", in: "group-override=admins", want: Identifier{Type: IDGroupOverride, Name: "admins"}},
		{name: "user with at sign", in: "user=bob@example.com", want: Identifier{Type: IDUser, Name: "bob@example.com"}},
		{name: "empty user", in: "user=", wantErr: true},
		{name: "empty group", in: "group=", wantErr: true},
		{name: "empty group-override", in: "group-override=", wantErr: true},
		{name: "unknown", in: "everyone", wantErr: true},
		{name: "blank", in: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseIdentifier(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if rt := got.String(); rt != tc.in {
				t.Fatalf("round-trip: got %q, want %q", rt, tc.in)
			}
		})
	}
}

func TestIdentifier_StringInvalid(t *testing.T) {
	if (Identifier{}).String() != "" {
		t.Fatal("IDInvalid should serialise to empty string")
	}
}

func TestParseEntry(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantOK   bool
		want     Entry
		wantErr  bool
		wantLine string // round-trip expectation when not blank
	}{
		{
			name: "positive simple", in: "user=alice lrsw", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "alice"}, Rights: "lrsw"},
			wantLine: "user=alice lrsw",
		},
		{
			name: "negative", in: "-user=mallory lrwa", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "mallory"}, Rights: "lrwa", Negative: true},
			wantLine: "-user=mallory lrwa",
		},
		{
			name: "anyone with rights", in: "anyone l", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDAnyone}, Rights: "l"},
			wantLine: "anyone l",
		},
		{
			name: "owner full canonical", in: "owner lrswipkxtea", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDOwner}, Rights: FullRights},
			wantLine: "owner lrswipkxtea",
		},
		{
			name: "obsolete c expanded on parse", in: "user=bob lc", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "bob"}, Rights: "lk"},
			wantLine: "user=bob lk",
		},
		{
			name: "extra whitespace tolerated", in: "  user=carol   lrsw  ", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "carol"}, Rights: "lrsw"},
			wantLine: "user=carol lrsw",
		},
		{
			name: "identifier only — explicit no rights", in: "user=dan", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "dan"}, Rights: ""},
			wantLine: "user=dan ",
		},
		{name: "blank line skipped", in: "", wantOK: false},
		{name: "whitespace only skipped", in: "   \t  ", wantOK: false},
		{name: "comment skipped", in: "# this is a comment", wantOK: false},
		{name: "indented comment skipped", in: "   # nope", wantOK: false},
		{name: "too many fields", in: "user=eve lrs trailing", wantErr: true},
		{name: "invalid right", in: "user=eve LRS", wantErr: true},
		{name: "invalid identifier", in: "everyone lrs", wantErr: true},
		// The identifier may contain spaces; rights are the LAST field. The
		// reference round-trips these, and one such line must not poison the
		// file (#1140 item 3).
		{
			name: "spaced identifier with rights", in: "user=John Smith lrw", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "John Smith"}, Rights: "lrw"},
			wantLine: "user=John Smith lrw",
		},
		{
			name: "spaced identifier, explicit no rights", in: "user=John Smith ", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "John Smith"}, Rights: ""},
			wantLine: "user=John Smith ",
		},
		{
			name: "negative spaced identifier", in: "-user=John Smith a", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "John Smith"}, Rights: "a", Negative: true},
			wantLine: "-user=John Smith a",
		},
		// anonymous is the reference's spelling of anyone; canonical output
		// normalises it (#1140 item 4).
		{
			name: "anonymous is anyone", in: "anonymous lr", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDAnyone}, Rights: "lr"},
			wantLine: "anyone lr",
		},
		// A middle token that happens to spell valid rights is absorbed into
		// the spaced identifier -- the direct consequence of allowing spaces,
		// pinned as decided: rejecting it would need a heuristic ("does a
		// token parse as rights?") that misfires on names like "area 51".
		{
			name: "rights-looking middle token absorbed", in: "user=bob lr lrs", wantOK: true,
			want:     Entry{Identifier: Identifier{Type: IDUser, Name: "bob lr"}, Rights: "lrs"},
			wantLine: "user=bob lr lrs",
		},
		// What the line-oriented format cannot carry is refused (#1140 item 2).
		{name: "control character in identifier", in: "user=ev\x01l lr", wantErr: true},
		{name: "newline smuggled into identifier", in: "user=a\nanyone lr", wantErr: true},
		{name: "invalid UTF-8 identifier", in: "user=\xff\xfe lr", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := ParseEntry(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v ok=%v", got, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if rt := got.String(); rt != tc.wantLine {
				t.Fatalf("round-trip: got %q, want %q", rt, tc.wantLine)
			}
		})
	}
}

func TestParseACL(t *testing.T) {
	body := `# top-level comment
owner lrswipkxtea

user=alice lrsw
# inline comment line
-user=mallory lrwa
group=staff lr
`
	got, err := ParseACLString(body)
	if err != nil {
		t.Fatalf("ParseACL: %v", err)
	}
	want := ACL{
		{Identifier: Identifier{Type: IDOwner}, Rights: FullRights},
		{Identifier: Identifier{Type: IDUser, Name: "alice"}, Rights: "lrsw"},
		{Identifier: Identifier{Type: IDUser, Name: "mallory"}, Rights: "lrwa", Negative: true},
		{Identifier: Identifier{Type: IDGroup, Name: "staff"}, Rights: "lr"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	encoded := got.String()
	roundTrip, err := ParseACLString(encoded)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if !reflect.DeepEqual(roundTrip, want) {
		t.Fatalf("round-trip ACL = %+v, want %+v", roundTrip, want)
	}
}

func TestParseACL_ErrorAnnotatesLine(t *testing.T) {
	body := "owner lrswipkxtea\n\n# fine\nuser=eve LRS\n"
	_, err := ParseACLString(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "line 4") {
		t.Errorf("error %q should mention line 4", err)
	}
}

func TestParseACL_OversizedLine(t *testing.T) {
	huge := "user=eve " + strings.Repeat("l", 2*1024*1024) + "\n"
	_, err := ParseACLString(huge)
	if err == nil {
		t.Fatal("expected scanner error for oversized line")
	}
}

func TestACL_StringEmpty(t *testing.T) {
	if got := (ACL{}).String(); got != "" {
		t.Errorf("empty ACL should serialise to empty string, got %q", got)
	}
}

func TestACL_StringTrailingNewline(t *testing.T) {
	acl := ACL{{Identifier: Identifier{Type: IDOwner}, Rights: "lr"}}
	got := acl.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("non-empty ACL should end with newline, got %q", got)
	}
}

func TestACL_Sorted(t *testing.T) {
	in := ACL{
		{Identifier: Identifier{Type: IDUser, Name: "carol"}, Rights: "lr"},
		{Identifier: Identifier{Type: IDOwner}, Rights: FullRights},
		{Identifier: Identifier{Type: IDUser, Name: "alice"}, Rights: "lr"},
		{Identifier: Identifier{Type: IDUser, Name: "alice"}, Rights: "wa", Negative: true},
		{Identifier: Identifier{Type: IDAnyone}, Rights: "l"},
		{Identifier: Identifier{Type: IDGroup, Name: "staff"}, Rights: "lr"},
	}
	got := in.Sorted()
	wantOrder := []Identifier{
		{Type: IDAnyone},
		{Type: IDOwner},
		{Type: IDUser, Name: "alice"}, // positive first
		{Type: IDUser, Name: "alice"}, // negative after
		{Type: IDUser, Name: "carol"},
		{Type: IDGroup, Name: "staff"},
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("length = %d, want %d", len(got), len(wantOrder))
	}
	for i, e := range got {
		if e.Identifier != wantOrder[i] {
			t.Errorf("entry %d: identifier = %+v, want %+v", i, e.Identifier, wantOrder[i])
		}
	}
	// positive/negative order check on the alice pair
	if got[2].Negative || !got[3].Negative {
		t.Errorf("alice pair: want positive then negative, got %+v / %+v", got[2], got[3])
	}
	// original slice untouched
	if in[0].Identifier.Name != "carol" {
		t.Errorf("Sorted mutated input slice")
	}
}

func TestACL_Effective(t *testing.T) {
	owner := MustParseRights("lrswipkxtea")
	tests := []struct {
		name    string
		acl     ACL
		user    string
		isOwner bool
		want    Rights
	}{
		{
			// Strong owner grant (§3.7): the owner resolves to FullRights
			// regardless of entries, so a reduced user= entry for the owner does
			// NOT cap them -- a shared/personal namespace has no second owner to
			// undo a SETACL that locked the first out.
			name: "owner: a reduced user= entry for self does not cap the owner",
			acl:  ACL{{Identifier: Identifier{Type: IDUser, Name: "alice"}, Rights: "lr"}},
			user: "alice", isOwner: true,
			want: owner,
		},
		{
			name: "owner on empty ACL still gets everything",
			acl:  nil,
			user: "alice", isOwner: true,
			want: owner,
		},
		{
			// anyone (tier 0) is below the owner default (tier 3) — owner keeps full.
			name: "owner: anyone entry does not lower the owner default",
			acl:  ACL{{Identifier: Identifier{Type: IDAnyone}, Rights: "l"}},
			user: "alice", isOwner: true,
			want: owner,
		},
		{
			name: "non-owner with no entries gets nothing",
			acl:  ACL{{Identifier: Identifier{Type: IDUser, Name: "alice"}, Rights: "lr"}},
			user: "bob", isOwner: false,
			want: "",
		},
		{
			name: "non-owner explicit user=<self>",
			acl:  ACL{{Identifier: Identifier{Type: IDUser, Name: "bob"}, Rights: "lrws"}},
			user: "bob", isOwner: false,
			want: "lrsw",
		},
		{
			// user= (tier 4) replaces anyone (tier 0), not merges with it.
			name: "non-owner user= replaces anyone",
			acl: ACL{
				{Identifier: Identifier{Type: IDAnyone}, Rights: "l"},
				{Identifier: Identifier{Type: IDUser, Name: "bob"}, Rights: "rs"},
			},
			user: "bob", isOwner: false,
			want: "rs",
		},
		{
			name: "non-owner authenticated grants",
			acl: ACL{
				{Identifier: Identifier{Type: IDAuthenticated}, Rights: "lr"},
			},
			user: "bob", isOwner: false,
			want: "lr",
		},
		{
			name: "negative subtracts from positive",
			acl: ACL{
				{Identifier: Identifier{Type: IDAnyone}, Rights: "lrs"},
				{Identifier: Identifier{Type: IDAnyone}, Rights: "s", Negative: true},
			},
			user: "bob", isOwner: false,
			want: "lr",
		},
		{
			name: "negative user= subtracts from positive anyone",
			acl: ACL{
				{Identifier: Identifier{Type: IDAnyone}, Rights: "lrs"},
				{Identifier: Identifier{Type: IDUser, Name: "bob"}, Rights: "r", Negative: true},
			},
			user: "bob", isOwner: false,
			want: "ls",
		},
		{
			name: "negative alone yields empty",
			acl: ACL{
				{Identifier: Identifier{Type: IDUser, Name: "bob"}, Rights: "r", Negative: true},
			},
			user: "bob", isOwner: false,
			want: "",
		},
		{
			name: "group= without membership has no effect",
			acl: ACL{
				{Identifier: Identifier{Type: IDGroup, Name: "staff"}, Rights: "lr"},
			},
			user: "bob", isOwner: false,
			want: "",
		},
		{
			name: "explicit owner identifier ignored for non-owner",
			acl: ACL{
				{Identifier: Identifier{Type: IDOwner}, Rights: "lr"},
			},
			user: "bob", isOwner: false,
			want: "",
		},
		{
			name: "user= for someone else does not match",
			acl: ACL{
				{Identifier: Identifier{Type: IDUser, Name: "alice"}, Rights: "lrws"},
			},
			user: "bob", isOwner: false,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.acl.Effective(tc.user, nil, tc.isOwner)
			if got != tc.want {
				t.Errorf("Effective = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestACL_Effective_Groups(t *testing.T) {
	tests := []struct {
		name    string
		acl     ACL
		user    string
		groups  []string
		isOwner bool
		want    Rights
	}{
		{
			name: "group= matches when user is a member",
			acl: ACL{
				{Identifier: Identifier{Type: IDGroup, Name: "staff"}, Rights: "lr"},
			},
			user: "bob", groups: []string{"staff"},
			want: "lr",
		},
		{
			name: "group= no effect when not a member",
			acl: ACL{
				{Identifier: Identifier{Type: IDGroup, Name: "staff"}, Rights: "lr"},
			},
			user: "bob", groups: []string{"other"},
			want: "",
		},
		{
			// group= (tier 2) replaces anyone (tier 0) for a positive grant.
			name: "group= replaces anyone",
			acl: ACL{
				{Identifier: Identifier{Type: IDAnyone}, Rights: "l"},
				{Identifier: Identifier{Type: IDGroup, Name: "staff"}, Rights: "rs"},
			},
			user: "bob", groups: []string{"staff"},
			want: "rs",
		},
		{
			name: "negative group= subtracts from positive anyone",
			acl: ACL{
				{Identifier: Identifier{Type: IDAnyone}, Rights: "lrs"},
				{Identifier: Identifier{Type: IDGroup, Name: "restricted"}, Rights: "s", Negative: true},
			},
			user: "bob", groups: []string{"restricted"},
			want: "lr",
		},
		{
			name: "group-override= replaces base when member",
			acl: ACL{
				{Identifier: Identifier{Type: IDUser, Name: "bob"}, Rights: "l"},
				{Identifier: Identifier{Type: IDGroupOverride, Name: "admins"}, Rights: "lrswipkxtea"},
			},
			user: "bob", groups: []string{"admins"},
			want: "lrswipkxtea",
		},
		{
			name: "group-override= no effect when not a member",
			acl: ACL{
				{Identifier: Identifier{Type: IDUser, Name: "bob"}, Rights: "lr"},
				{Identifier: Identifier{Type: IDGroupOverride, Name: "admins"}, Rights: "lrswipkxtea"},
			},
			user: "bob", groups: nil,
			want: "lr",
		},
		{
			name: "multiple groups all contribute",
			acl: ACL{
				{Identifier: Identifier{Type: IDGroup, Name: "readers"}, Rights: "lr"},
				{Identifier: Identifier{Type: IDGroup, Name: "writers"}, Rights: "sw"},
			},
			user: "bob", groups: []string{"readers", "writers"},
			want: "lrsw",
		},
		{
			name: "group-override replaces even when base has more rights",
			acl: ACL{
				{Identifier: Identifier{Type: IDAnyone}, Rights: "lrswipkxtea"},
				{Identifier: Identifier{Type: IDGroupOverride, Name: "readonly"}, Rights: "lr"},
			},
			user: "bob", groups: []string{"readonly"},
			want: "lr",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.acl.Effective(tc.user, tc.groups, tc.isOwner)
			if got != tc.want {
				t.Errorf("Effective = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseACL_ReadErrorWrapped(t *testing.T) {
	r := &errReader{err: io.ErrUnexpectedEOF}
	_, err := ParseACL(r)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error should wrap io.ErrUnexpectedEOF, got %v", err)
	}
}

type errReader struct{ err error }

func (e *errReader) Read(_ []byte) (int, error) { return 0, e.err }

func TestACL_EffectiveWithGlobal(t *testing.T) {
	u := func(name, rights string, neg bool) Entry {
		return Entry{Identifier: Identifier{Type: IDUser, Name: name}, Rights: MustParseRights(rights), Negative: neg}
	}
	anyone := func(rights string, neg bool) Entry {
		return Entry{Identifier: Identifier{Type: IDAnyone}, Rights: MustParseRights(rights), Negative: neg}
	}
	tests := []struct {
		name          string
		local, global ACL
		user          string
		isOwner       bool
		want          string
	}{
		{"owner default full when no global matches", nil, nil, "alice", true, "lrswipkxtea"},
		{"the owner beats a global negative", ACL{u("bob", "lr", false)}, ACL{anyone("lr", true)}, "bob", true, "lrswipkxtea"},
		{"global only, no local", nil, ACL{u("bob", "lr", false)}, "bob", false, "lr"},
		{"local only, global does not match", ACL{u("bob", "lr", false)}, ACL{u("carol", "lrswi", false)}, "bob", false, "lr"},
		// A matching global positive REPLACES the local mask rather than adding
		// to it: the globals are a tier ladder above the local ACL, not a
		// second opinion merged into it. This case used to expect "lri" and
		// that expectation was the defect written down (#1117).
		{"global positive replaces the local grant", ACL{u("bob", "lr", false)}, ACL{anyone("i", false)}, "bob", false, "i"},
		{"global negative revokes a local grant", ACL{u("bob", "lrs", false)}, ACL{u("bob", "s", true)}, "bob", false, "lr"},
		{"global matching resets local negative", ACL{anyone("lr", true), u("bob", "lr", false)}, ACL{anyone("i", false)}, "bob", false, "i"},
		// The half that fails open, and the reason this one is worth fixing
		// first: a local negative used to be discarded whenever ANY global
		// entry matched, so an unrelated global grant re-granted what the
		// mailbox had explicitly revoked.
		{"an unrelated global does not re-grant what the local ACL revoked",
			ACL{u("alice", "lra", false), u("alice", "a", true)}, ACL{anyone("l", false)}, "alice", false, "l"},
		// A global that only revokes leaves the local positives standing: it
		// spoke about the negative mask, so only that mask is replaced.
		{"global negative alone does not blank the local grant",
			ACL{u("bob", "lrs", false)}, ACL{anyone("s", true)}, "bob", false, "lr"},
		{"neither local nor global", nil, nil, "bob", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveWithGlobal(tc.local, tc.global, tc.user, nil, tc.isOwner)
			if got != MustParseRights(tc.want) {
				t.Errorf("EffectiveWithGlobal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestACL_EffectiveLadder locks the RFC 4314 identifier specificity ladder:
// anyone < authenticated < group= < owner < user= < group-override=, where a
// positive higher tier replaces lower tiers and a negative one only subtracts.
// The ladder governs non-owners; the owner is resolved off the ladder entirely
// (strong grant, §3.7) and sits above every tier -- the owner row below pins
// that a higher tier does not restrict them.
func TestACL_EffectiveLadder(t *testing.T) {
	e := func(t IdentifierType, name, rights string, neg bool) Entry {
		return Entry{Identifier: Identifier{Type: t, Name: name}, Rights: MustParseRights(rights), Negative: neg}
	}
	tests := []struct {
		name    string
		acl     ACL
		user    string
		groups  []string
		isOwner bool
		want    string
	}{
		{"user= replaces group=", ACL{e(IDGroup, "staff", "lrs", false), e(IDUser, "bob", "l", false)}, "bob", []string{"staff"}, false, "l"},
		{"group-override= replaces user=", ACL{e(IDUser, "bob", "l", false), e(IDGroupOverride, "admin", "lrswi", false)}, "bob", []string{"admin"}, false, "lrswi"},
		{"two group= entries merge within their tier", ACL{e(IDGroup, "a", "lr", false), e(IDGroup, "b", "si", false)}, "bob", []string{"a", "b"}, false, "lrsi"},
		{"negative user= subtracts, keeps anyone's positive", ACL{e(IDAnyone, "", "lrs", false), e(IDUser, "bob", "s", true)}, "bob", nil, false, "lr"},
		{"group-override= does not restrict the owner (strong grant)", ACL{e(IDGroupOverride, "locked", "lr", false)}, "alice", []string{"locked"}, true, "lrswipkxtea"},
		{"authenticated replaces anyone", ACL{e(IDAnyone, "", "l", false), e(IDAuthenticated, "", "rs", false)}, "bob", nil, false, "rs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.acl.Effective(tc.user, tc.groups, tc.isOwner); got != MustParseRights(tc.want) {
				t.Errorf("Effective = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidIdentifier(t *testing.T) {
	long := strings.Repeat("a", 1025)
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"plain", "user=alice", true},
		{"spaced", "user=John Smith", true},
		{"utf8 name", "user=Ольга", true},
		{"exactly 1024", "user=" + strings.Repeat("a", 1019), true},
		{"over 1024", "user=" + long, false},
		{"control char", "user=a\x01b", false},
		{"tab", "user=a\tb", false},
		{"del", "user=a\x7fb", false},
		{"invalid utf8", "user=\xff", false},
	}
	for _, c := range cases {
		if err := ValidIdentifier(c.in); (err == nil) != c.ok {
			t.Errorf("%s: ValidIdentifier(%q) err=%v, want ok=%v", c.name, c.in, err, c.ok)
		}
	}
}

// One malformed line fails the whole file, deliberately: fail-closed beats
// serving a partial ACL (docs/OWNER_SHARED_NS.md 7.7). Pinned so it is not
// "fixed" later by someone matching the reference's log-and-keep.
func TestParseACL_OneBadLineFailsClosed(t *testing.T) {
	body := "user=alice lr\nuser=bob !!\nuser=carol lr\n"
	if _, err := ParseACL(strings.NewReader(body)); err == nil {
		t.Fatal("a file with one malformed line parsed; want fail-closed")
	}
}
