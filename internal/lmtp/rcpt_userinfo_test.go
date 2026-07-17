package lmtp

import (
	"context"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestResolveRcptUserInfo(t *testing.T) {
	udb := func(_ context.Context, u string) (*mailbox.UserInfo, error) {
		return &mailbox.UserInfo{Username: u, Driver: "sdbox"}, nil
	}

	t.Run("cache hit wins", func(t *testing.T) {
		s := &session{opts: Options{UserdbLookup: udb}, rcptUserInfo: map[string]*mailbox.UserInfo{
			"bob@x": {Username: "bob@x", Driver: "mdbox"},
		}}
		if ui := s.resolveRcptUserInfo("bob@x", "bob@x"); ui.Driver != "mdbox" {
			t.Fatalf("driver = %q, want mdbox (cached)", ui.Driver)
		}
	})

	t.Run("cache miss re-resolves via userdb → driver stamped", func(t *testing.T) {
		s := &session{opts: Options{UserdbLookup: udb}} // rcptUserInfo nil
		if ui := s.resolveRcptUserInfo("bob@x", "bob@x"); ui.Driver != "sdbox" {
			t.Fatalf("driver = %q, want sdbox (re-resolved)", ui.Driver)
		}
	})

	t.Run("no userdb → bare resolver, no driver (global backend)", func(t *testing.T) {
		s := &session{opts: Options{}}
		if ui := s.resolveRcptUserInfo("c@x", "c@x"); ui.Driver != "" {
			t.Fatalf("driver = %q, want empty", ui.Driver)
		}
	})
}
