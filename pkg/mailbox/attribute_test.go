package mailbox

import "testing"

func TestParseAttrEntry(t *testing.T) {
	cases := []struct {
		in        string
		wantScope AttrScope
		wantName  string
		wantErr   bool
	}{
		{"/private/comment", AttrPrivate, "comment", false},
		{"/shared/admin", AttrShared, "admin", false},
		{"/private/vendor/yarilo/abc", AttrPrivate, "vendor/yarilo/abc", false},
		{"comment", 0, "", true},
		{"/other/foo", 0, "", true},
		{"", 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			scope, name, err := ParseAttrEntry(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if scope != tc.wantScope {
				t.Errorf("scope = %v, want %v", scope, tc.wantScope)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
		})
	}
}

func TestAttrKeyRoundtrip(t *testing.T) {
	guid := [16]byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	priv := AttrKey(AttrPrivate, guid, "comment")
	shared := AttrKey(AttrShared, guid, "comment")

	wantPriv := "priv/box/deadbeef0102030405060708090a0b0c/comment"
	wantShared := "shared/box/deadbeef0102030405060708090a0b0c/comment"

	if priv != wantPriv {
		t.Errorf("private key = %q, want %q", priv, wantPriv)
	}
	if shared != wantShared {
		t.Errorf("shared key = %q, want %q", shared, wantShared)
	}

	// Same entry name in different scopes must produce different keys.
	if priv == shared {
		t.Error("private and shared keys must differ for the same attrName")
	}
}

func TestServerAttrKeyVendorPrefix(t *testing.T) {
	guid := [16]byte{}
	for i := range guid {
		guid[i] = byte(i)
	}
	srv := ServerAttrKey(AttrPrivate, guid, "comment")
	mbx := AttrKey(AttrPrivate, guid, "comment")
	if srv == mbx {
		t.Fatal("server-scope key must differ from INBOX-scope key with the same attrName")
	}
	want := "priv/box/000102030405060708090a0b0c0d0e0f/vendor/yarilo/pvt/server/comment"
	if srv != want {
		t.Errorf("server key = %q, want %q", srv, want)
	}
}

func TestAttrPrefixIterability(t *testing.T) {
	guid := [16]byte{}
	prefix := AttrPrefix(AttrPrivate, guid)
	key := AttrKey(AttrPrivate, guid, "comment")
	if got := TrimAttrPrefix(key, prefix); got != "comment" {
		t.Errorf("trim(%q, %q) = %q, want %q", key, prefix, got, "comment")
	}
	if got := TrimAttrPrefix("not-our-key", prefix); got != "" {
		t.Errorf("trim of non-matching key returned %q, want empty", got)
	}
}

func TestFormatAttrEntry(t *testing.T) {
	if got := FormatAttrEntry(AttrPrivate, "comment"); got != "/private/comment" {
		t.Errorf("private format: %q", got)
	}
	if got := FormatAttrEntry(AttrShared, "admin"); got != "/shared/admin" {
		t.Errorf("shared format: %q", got)
	}
}

// SharedAttrKey: shared/public-namespace per-accessing-user keys ----------------

func TestSharedAttrKeyPrivateIsPerUser(t *testing.T) {
	// Same folder GUID, same attrName, two different accessing users:
	// the priv/ key must differ so users cannot see each other's
	// private annotations on a shared folder.
	guid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	aliceKey := SharedAttrKey(AttrPrivate, guid, "alice@example.com", "comment")
	bobKey := SharedAttrKey(AttrPrivate, guid, "bob@example.com", "comment")
	if aliceKey == bobKey {
		t.Fatalf("private shared keys collided across users: %q", aliceKey)
	}
	// Both must include the shared folder's GUID and the "u-" prefix.
	if !contains(aliceKey, "0102030405060708090a0b0c0d0e0f10/u-") {
		t.Errorf("alice key shape wrong: %q", aliceKey)
	}
}

func TestSharedAttrKeySharedIsGlobal(t *testing.T) {
	// shared/ scope on a shared folder remains visible to everyone —
	// the accessing-user dimension is NOT applied.
	guid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	aliceKey := SharedAttrKey(AttrShared, guid, "alice@example.com", "admin")
	bobKey := SharedAttrKey(AttrShared, guid, "bob@example.com", "admin")
	if aliceKey != bobKey {
		t.Errorf("shared/ scope must be global on shared folder: alice=%q bob=%q", aliceKey, bobKey)
	}
}

func TestSharedAttrPrefixMatchesKey(t *testing.T) {
	// SharedAttrPrefix + attrName must equal SharedAttrKey, so
	// iterating under the prefix and stripping it returns just the
	// attr name.
	guid := [16]byte{0xab, 0xcd}
	for _, scope := range []AttrScope{AttrPrivate, AttrShared} {
		key := SharedAttrKey(scope, guid, "alice@example.com", "comment")
		prefix := SharedAttrPrefix(scope, guid, "alice@example.com")
		if got := TrimAttrPrefix(key, prefix); got != "comment" {
			t.Errorf("scope=%v: trim(%q, %q)=%q want=comment", scope, key, prefix, got)
		}
	}
}

func TestSharedAttrKeyHashIsCaseSensitive(t *testing.T) {
	guid := [16]byte{}
	upper := SharedAttrKey(AttrPrivate, guid, "Alice@example.com", "x")
	lower := SharedAttrKey(AttrPrivate, guid, "alice@example.com", "x")
	if upper == lower {
		t.Error("user hash must be case-sensitive")
	}
}

// helper: simple substring contains (avoids importing strings just for tests)
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
