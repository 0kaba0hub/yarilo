package imap

import "testing"

func TestExtractOwner(t *testing.T) {
	tmpl := NamespaceSpec{Type: NamespaceShared, Prefix: "user/%u/", Separator: '/'}
	fixed := NamespaceSpec{Type: NamespaceShared, Prefix: "Public/", Separator: '/'}

	cases := []struct {
		name      string
		spec      NamespaceSpec
		in        string
		wantOwner string
		wantRel   string
		wantOK    bool
	}{
		{"owner and folder", tmpl, "user/alice/Sent", "alice", "Sent", true},
		{"bare name is the owner's INBOX", tmpl, "user/alice", "alice", "INBOX", true},
		{"trailing separator is also INBOX", tmpl, "user/alice/", "alice", "INBOX", true},
		{"nested folder keeps its path", tmpl, "user/alice/Work/2026", "alice", "Work/2026", true},

		// Not owner-templated: a fixed prefix has no owner to extract.
		{"fixed namespace is not templated", fixed, "Public/News", "", "", false},

		// The security cases: a malformed owner segment resolves to nobody, not
		// to the requesting user. "" is what keeps isOwner honest.
		{"traversal owner is rejected", tmpl, "user/../Sent", "", "", false},
		{"dot owner is rejected", tmpl, "user/./Sent", "", "", false},
		{"empty owner is rejected", tmpl, "user//Sent", "", "", false},

		// Not under this namespace at all.
		{"name outside the prefix", tmpl, "other/alice/Sent", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, rel, ok := extractOwner(tc.spec, tc.in)
			if ok != tc.wantOK || owner != tc.wantOwner || rel != tc.wantRel {
				t.Errorf("extractOwner(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, owner, rel, ok, tc.wantOwner, tc.wantRel, tc.wantOK)
			}
		})
	}
}

// The decision that matters for isOwner's honesty, asserted on its own: a name
// that does not name a real owner must never yield the session user. Every
// rejection returns an empty owner, so h.owner stays "" and the mailbox is
// owned by nobody -- not by whoever asked (#544/B1).
func TestExtractOwnerRejectionYieldsNobodyNotTheCaller(t *testing.T) {
	tmpl := NamespaceSpec{Type: NamespaceShared, Prefix: "user/%u/", Separator: '/'}
	for _, in := range []string{"user/../x", "user/./x", "user//x", "other/alice", "Public/News"} {
		if owner, _, ok := extractOwner(tmpl, in); ok || owner != "" {
			t.Errorf("extractOwner(%q) = (%q, ok=%v); a non-owner must resolve to \"\", never a user", in, owner, ok)
		}
	}
}
