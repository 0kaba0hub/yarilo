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
