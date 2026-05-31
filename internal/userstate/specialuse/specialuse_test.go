package specialuse

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

func TestSpecialUseDefaultsAndOverrides(t *testing.T) {
	home := t.TempDir()
	defaults := map[string]string{
		"Sent":   `\Sent`,
		"Drafts": `\Drafts`,
	}
	store := New(home, "alice@example.com", "owner", nil, defaults)

	cases := []struct {
		name     string
		folder   string
		wantAttr imaplib.MailboxAttr
	}{
		{"default applies before override", "Sent", `\Sent`},
		{"unknown folder returns empty", "Random", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := store.Get(c.folder); got != c.wantAttr {
				t.Errorf("got %q want %q", got, c.wantAttr)
			}
		})
	}

	if err := store.Set("Sent", `\Junk`); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := store.Get("Sent"); got != `\Junk` {
		t.Errorf("after override Get(Sent)=%q want \\Junk", got)
	}

	if err := store.Delete("Sent"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := store.Get("Sent"); got != `\Sent` {
		t.Errorf("after delete Get(Sent)=%q want default \\Sent", got)
	}
}

func TestSpecialUseSnapshotMissingFile(t *testing.T) {
	home := t.TempDir()
	store := New(home, "alice", "owner", nil, nil)
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) != 0 {
		t.Errorf("snapshot=%v want empty", snap)
	}
}
