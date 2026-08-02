package acl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func TestNewGlobalAndFor(t *testing.T) {
	g, err := NewGlobal([]config.GlobalACLRule{
		{Mailbox: "*", Entries: []config.GlobalACLEntry{
			{Identifier: "user=admin@d.test", Rights: "lrswipkxtea"},
			{Identifier: "anyone", Rights: "-lr"}, // negative
		}},
		{Mailbox: "Public/News", Entries: []config.GlobalACLEntry{
			{Identifier: "anyone", Rights: "lr"},
		}},
	})
	if err != nil {
		t.Fatalf("NewGlobal: %v", err)
	}
	// "*" applies to every folder; Public/News additionally gets its own.
	if got := g.For("INBOX"); len(got) != 2 {
		t.Errorf("For(INBOX) = %d entries, want 2 (the * rule)", len(got))
	}
	news := g.For("Public/News")
	if len(news) != 3 {
		t.Errorf("For(Public/News) = %d entries, want 3 (* + exact)", len(news))
	}
	// Negative flag parsed from the leading "-".
	var sawNeg bool
	for _, e := range g.For("INBOX") {
		if e.Identifier.Type == mailbox.IDAnyone && e.Negative {
			sawNeg = true
		}
	}
	if !sawNeg {
		t.Error("anyone -lr should parse as a negative entry")
	}
	// nil *Global is safe.
	var nilG *Global
	if nilG.For("INBOX") != nil {
		t.Error("nil Global.For should return nil")
	}
}

func TestStore_GlobalMergeAndGlobalsOnly(t *testing.T) {
	global, err := NewGlobal([]config.GlobalACLRule{
		{Mailbox: "*", Entries: []config.GlobalACLEntry{
			{Identifier: "user=bob", Rights: "lr"},
		}},
	})
	if err != nil {
		t.Fatalf("NewGlobal: %v", err)
	}

	// Global grants bob lr on a mailbox with no local ACL.
	s := New(t.TempDir(), "", "mdbox", "/", "alice", "test", Policy{Global: global}, nil)
	if got, _ := s.EffectiveFor("Projects", "bob", nil, false, '/'); got != "lr" {
		t.Errorf("global grant: EffectiveFor(Projects, bob)=%q, want lr", got)
	}

	// Local ACL adds on top of the global one.
	home := t.TempDir()
	s2 := New(home, "", "mdbox", "/", "alice", "test", Policy{Global: global}, nil)
	if err := s2.Set("Projects", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "i"},
	}); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if got, _ := s2.EffectiveFor("Projects", "bob", nil, false, '/'); got != "lri" {
		t.Errorf("local+global: EffectiveFor(Projects, bob)=%q, want lri", got)
	}

	// globals_only ignores the local ACL entirely.
	s3 := New(home, "", "mdbox", "/", "alice", "test", Policy{Global: global, GlobalsOnly: true}, nil)
	if got, _ := s3.EffectiveFor("Projects", "bob", nil, false, '/'); got != "lr" {
		t.Errorf("globals_only: EffectiveFor(Projects, bob)=%q, want lr (local ignored)", got)
	}
	// Confirm the local file exists so globals_only is truly ignoring it.
	if _, err := os.Stat(filepath.Join(home, "mailboxes", "Projects", "dbox-Mails", FileName)); err != nil {
		t.Fatalf("local ACL file should exist: %v", err)
	}
}
