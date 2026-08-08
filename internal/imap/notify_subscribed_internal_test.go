package imap

import (
	"reflect"
	"sort"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/userstate/subs"
)

// The SUBSCRIBED watch set is compared against personal-relative folder names,
// while the store's keys are client-visible names: user/alice/Sent and
// Public/Foo live in the same file as INBOX. The expansion must keep only the
// names the personal namespace can actually match — and must not drop personal
// folders that merely share a head with another namespace's prefix.
func TestExpandNotifySpec_SubscribedKeepsOnlyPersonalNames(t *testing.T) {
	store := subs.New(t.TempDir(), "subscriptions", "alice", "test", nil)
	for _, key := range []string{
		"INBOX",
		"Work",
		"user/alice/Sent", // owner-templated namespace, visible name
		"Public/Foo",      // fixed public prefix
		"userland/Notes",  // personal folder that shares a head with user/
		"Publicity/Bar",   // personal folder that shares a head with Public/
	} {
		if err := store.Add(key); err != nil {
			t.Fatal(err)
		}
	}

	off := false
	personalSpec := NamespaceSpec{Type: NamespacePersonal, Prefix: "", Separator: '/', List: ListYes}
	templSpec := NamespaceSpec{Type: NamespaceShared, Prefix: "user/%u/", Separator: '/', List: ListYes}
	publicSpec := NamespaceSpec{Type: NamespaceShared, Prefix: "Public/", Separator: '/', List: ListYes, Subscriptions: &off}

	primary := &nsHandle{name: "personal", spec: personalSpec, subs: store}
	s := &session{
		srv: &Server{opts: Options{Namespaces: []NamespaceSpec{personalSpec, templSpec, publicSpec}}},
		namespaces: map[string]*nsHandle{
			"":        primary,
			"Public/": {name: "public", spec: publicSpec},
		},
		primary: primary,
	}

	got := s.expandNotifySpec(imaplib.NotifyItem{MailboxSpec: imaplib.NotifyMailboxSpecSubscribed}, nil)
	sort.Strings(got)
	want := []string{"INBOX", "Publicity/Bar", "Work", "userland/Notes"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expandNotifySpec(SUBSCRIBED) = %v, want %v", got, want)
	}
}

func TestNamesPrimaryFolder(t *testing.T) {
	personalSpec := NamespaceSpec{Type: NamespacePersonal, Prefix: "", Separator: '/', List: ListYes}
	templSpec := NamespaceSpec{Type: NamespaceShared, Prefix: "user/%u/", Separator: '/', List: ListYes}
	publicSpec := NamespaceSpec{Type: NamespaceShared, Prefix: "Public/", Separator: '/', List: ListYes}

	primary := &nsHandle{name: "personal", spec: personalSpec}
	s := &session{
		srv: &Server{opts: Options{Namespaces: []NamespaceSpec{personalSpec, templSpec, publicSpec}}},
		namespaces: map[string]*nsHandle{
			"":        primary,
			"Public/": {name: "public", spec: publicSpec},
			"#other":  nil, // declared-only slot must not panic
		},
		primary: primary,
	}

	cases := []struct {
		name string
		want bool
	}{
		{"INBOX", true},
		{"Work/2026", true},
		{"Public/Foo", false},
		{"Publicity/Bar", true}, // shares letters with Public/, not the prefix
		{"user/alice/Sent", false},
		{"user/alice", false}, // bare owner name still addresses the namespace
		{"userland/Notes", true},
		{"user", true}, // no owner segment: not under the template
	}
	for _, tc := range cases {
		if got := s.namesPrimaryFolder(tc.name); got != tc.want {
			t.Errorf("namesPrimaryFolder(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
