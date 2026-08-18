package jmap

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// serverWithFolders builds an account with n folders beside INBOX, which is
// what the state has to walk on every Email/get and Email/set.
func serverWithFolders(tb testing.TB, n int) (*Server, *userHandle) {
	tb.Helper()
	s, _, _ := storedServerWithMessageAt(&testing.T{}, setTestMessage, 0)
	h, err := s.opts.Storage.open(testUser)
	if err != nil {
		tb.Fatalf("open user: %v", err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Folder%03d", i)
		if err := h.box.Create(name); err != nil {
			tb.Fatalf("create %s: %v", name, err)
		}
		f, err := h.idx.OpenFolder(name, 0)
		if err != nil {
			tb.Fatalf("open %s: %v", name, err)
		}
		if err := h.idx.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: 1, Filename: "m.eml", Size: 10,
		}); err != nil {
			tb.Fatalf("append %s: %v", name, err)
		}
	}
	h.close()
	tb.Cleanup(func() {})
	fresh, err := s.opts.Storage.open(testUser)
	if err != nil {
		tb.Fatalf("reopen: %v", err)
	}
	return s, fresh
}

// The cost this measures is the one a polling client pays on every request:
// until push lands, polling is the normal pattern (#1343).
func BenchmarkEmailState(b *testing.B) {
	for _, folders := range []int{30, 300} {
		b.Run(fmt.Sprintf("%d-folders", folders), func(b *testing.B) {
			s, _ := serverWithFolders(b, folders)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// A fresh handle per iteration, because that is what a request
				// gets: the handle does not outlive it, so every request starts
				// from a cold index.
				h, err := s.opts.Storage.open(testUser)
				if err != nil {
					b.Fatalf("open: %v", err)
				}
				if _, err := s.emailState(h); err != nil {
					b.Fatalf("emailState: %v", err)
				}
				h.close()
			}
		})
	}
}

// BenchmarkStateFloor is what remains when every marker is cached: opening the
// per-request handle, listing the folders, and two stats each. It bounds what
// the cache can achieve, so a later reader can tell "the cache is not working"
// from "this is the floor".
func BenchmarkStateFloor(b *testing.B) {
	for _, folders := range []int{30, 300} {
		b.Run(fmt.Sprintf("%d-folders", folders), func(b *testing.B) {
			s, _ := serverWithFolders(b, folders)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h, err := s.opts.Storage.open(testUser)
				if err != nil {
					b.Fatalf("open: %v", err)
				}
				entries, err := h.box.ListFolders()
				if err != nil {
					b.Fatalf("list: %v", err)
				}
				for _, e := range entries {
					if _, err := h.idx.FolderStamp(e.Name); err != nil {
						b.Fatalf("stamp: %v", err)
					}
				}
				h.close()
			}
		})
	}
}
