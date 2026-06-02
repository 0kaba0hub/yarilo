package protocol

import (
	"reflect"
	"strings"
	"testing"
)

func TestFields_SetGetHas(t *testing.T) {
	f := NewFields()
	if v, ok := f.Get("missing"); ok || v != "" {
		t.Errorf("Get on empty bag returned %q,%v; want \"\",false", v, ok)
	}
	f.Set("home", "/h/alice")
	v, ok := f.Get("home")
	if !ok || v != "/h/alice" {
		t.Errorf("Get(home) = %q,%v", v, ok)
	}
	if !f.Has("home") {
		t.Error("Has(home) should be true")
	}
	if f.Has("ghost") {
		t.Error("Has(ghost) should be false")
	}
}

func TestFields_SetOverwriteKeepsOrder(t *testing.T) {
	f := NewFields()
	f.Set("a", "1")
	f.Set("b", "2")
	f.Set("c", "3")
	f.Set("b", "22") // overwrite middle
	var keys []string
	f.Each(func(k, _ string) bool { keys = append(keys, k); return true })
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("overwrite changed insertion order: got %v, want %v", keys, want)
	}
	if v, _ := f.Get("b"); v != "22" {
		t.Errorf("Get(b) = %q, want 22", v)
	}
}

func TestFields_DeleteShiftsIndex(t *testing.T) {
	f := NewFields()
	f.Set("a", "1")
	f.Set("b", "2")
	f.Set("c", "3")
	f.Delete("b")
	if f.Has("b") {
		t.Error("Has(b) true after delete")
	}
	if v, _ := f.Get("c"); v != "3" {
		t.Errorf("Get(c) after deleting b = %q", v)
	}
	if f.Len() != 2 {
		t.Errorf("Len = %d, want 2", f.Len())
	}
	// Inserting a new key lands at the end, not in the removed slot.
	f.Set("d", "4")
	var keys []string
	f.Each(func(k, _ string) bool { keys = append(keys, k); return true })
	if !reflect.DeepEqual(keys, []string{"a", "c", "d"}) {
		t.Errorf("post-delete-insert order = %v", keys)
	}
}

func TestFields_NilSafe(t *testing.T) {
	var f *Fields
	if f.Has("x") {
		t.Error("Has on nil should be false")
	}
	if v, ok := f.Get("x"); ok || v != "" {
		t.Errorf("Get on nil returned %q,%v", v, ok)
	}
	if f.Len() != 0 {
		t.Errorf("Len on nil = %d", f.Len())
	}
	count := 0
	f.Each(func(_, _ string) bool { count++; return true })
	if count != 0 {
		t.Error("Each on nil iterated")
	}
	if got := f.WireForm(); got != nil {
		t.Errorf("WireForm on nil = %v, want nil", got)
	}
}

func TestFields_EachStopsEarly(t *testing.T) {
	f := NewFields()
	f.Set("a", "1")
	f.Set("b", "2")
	f.Set("c", "3")
	count := 0
	f.Each(func(k, _ string) bool {
		count++
		return k != "b" // stop after b
	})
	if count != 2 {
		t.Errorf("Each visited %d entries, want 2", count)
	}
}

func TestScopeOf(t *testing.T) {
	tests := []struct {
		key  string
		want Scope
	}{
		{"home", ScopePassdb},
		{"mail", ScopePassdb},
		{"user", ScopePassdb},
		{"userdb_uid", ScopeUserdb},
		{"userdb_quota_rule", ScopeUserdb},
		{"userdb_", ScopeUserdb}, // bare prefix still in scope
		{"auth_cache_key", ScopeInternal},
		{"auth_", ScopeInternal},
		// Unprefixed but starting with "auth" / "userdb" without
		// underscore stays passdb-scope — the underscore is part of
		// the marker.
		{"authenticated", ScopePassdb},
		{"userdb", ScopePassdb},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			if got := ScopeOf(tc.key); got != tc.want {
				t.Errorf("ScopeOf(%q) = %d, want %d", tc.key, got, tc.want)
			}
		})
	}
}

func TestFields_WireFormDropsInternal(t *testing.T) {
	f := NewFields()
	f.Set("user", "alice")
	f.Set("home", "/h/alice")
	f.Set("auth_cache_key", "secret-internal-state")
	f.Set("userdb_uid", "1001")
	f.Set("mail", "maildir:/m/alice")

	got := f.WireForm()
	for _, leak := range []string{"auth_cache_key", "secret-internal-state"} {
		for _, tok := range got {
			if strings.Contains(tok, leak) {
				t.Errorf("auth_* field leaked on wire: %q in %v", leak, got)
			}
		}
	}
	want := []string{
		"user=alice",
		"home=/h/alice",
		"userdb_uid=1001",
		"mail=maildir:/m/alice",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WireForm =\n got=%v\nwant=%v", got, want)
	}
}

func TestFields_WireFormEscapesUnsafeBytes(t *testing.T) {
	f := NewFields()
	f.Set("home", "/home/with\ttab\nand\\back")
	got := f.WireForm()
	if len(got) != 1 {
		t.Fatalf("expected 1 token, got %v", got)
	}
	want := `home=/home/with\ttab\nand\\back`
	if got[0] != want {
		t.Errorf("escape = %q, want %q", got[0], want)
	}
}

func TestFields_WireFormPreservesInsertionOrder(t *testing.T) {
	f := NewFields()
	for _, kv := range []struct{ k, v string }{
		{"c", "3"}, {"a", "1"}, {"b", "2"}, {"userdb_x", "ux"}, {"auth_drop", "d"},
	} {
		f.Set(kv.k, kv.v)
	}
	got := f.WireForm()
	want := []string{"c=3", "a=1", "b=2", "userdb_x=ux"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildAuthOK_FallbackTypedFields(t *testing.T) {
	res := &AuthResponse{
		Result:   AuthOK,
		Username: "alice",
		Home:     "/h/alice",
		MailLoc:  "maildir:/m/alice",
	}
	got := buildAuthOK("7", res)
	want := "OK\t7\tuser=alice\thome=/h/alice\tmail=maildir:/m/alice"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildAuthOK_PrefersFieldsBagWhenSet(t *testing.T) {
	bag := NewFields()
	bag.Set("user", "alice")
	bag.Set("home", "/h/alice")
	bag.Set("mail", "maildir:/m/alice")
	bag.Set("userdb_uid", "1001")
	bag.Set("auth_cache_key", "secret")
	res := &AuthResponse{
		Result:   AuthOK,
		Username: "alice",
		Home:     "ignored", // typed fields skipped when Fields is set
		MailLoc:  "ignored",
		Fields:   bag,
	}
	got := buildAuthOK("9", res)
	want := "OK\t9\tuser=alice\thome=/h/alice\tmail=maildir:/m/alice\tuserdb_uid=1001"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestBuildAuthOK_EmptyBagFallsBackToTyped(t *testing.T) {
	// An empty (non-nil) bag should still fall through to typed
	// fields so a passdb that allocates the bag but writes nothing
	// does not silently emit an OK with no fields.
	res := &AuthResponse{
		Result:   AuthOK,
		Username: "alice",
		Home:     "/h/alice",
		Fields:   NewFields(),
	}
	got := buildAuthOK("3", res)
	want := "OK\t3\tuser=alice\thome=/h/alice"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
