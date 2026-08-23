package jmap

import (
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The acceptance criterion #711 was written with, translated to the design that
// was actually built.
//
// The phase was designed around an append-only change journal, and its
// acceptance said "two concurrent writers produce a linear journal". There is
// no journal: a state is a description of every folder's markers and /changes
// diffs two descriptions. So the property that has to hold is the one the
// markers carry:
//
//	a delivery and a flag change landing at the same moment must BOTH appear in
//	a later diff, and neither may be reported as destroyed.
//
// The failure this guards against is not a crash. It is a marker read between
// two halves of somebody else's write -- a nextUID already bumped while the
// message it belongs to is not in the index yet, or flags written while the
// modseq that announces them is not. Either way a client is told something
// untrue and stops asking.
func TestConcurrentDeliveryAndFlagChangeBothAppear(t *testing.T) {
	// Repeated, because this is a race: one pass proves the happy interleaving
	// exists, not that the unhappy ones are absent. Run with -race.
	const rounds = 25
	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			// One lock service for the reader and both writers, as a
			// deployment has: with separate lockers the reader would never
			// coordinate with anybody, which makes the test stricter and
			// unfaithful at once.
			s, existingID, info, locker := serverSharingItsLocker(t)
			since := emailStateOf(t, s)

			// Two writers with their own handles on the same account, as LMTP
			// and an IMAP session are: separate processes reaching the same
			// index through the lock service.
			var wg sync.WaitGroup
			var deliveredID string
			var deliverErr, flagErr error

			wg.Add(2)
			go func() { // the delivery
				defer wg.Done()
				deliveredID, deliverErr = deliverAnother(info, locker, 2)
			}()
			go func() { // the flag change
				defer wg.Done()
				flagErr = flagExisting(info, locker, 1)
			}()
			wg.Wait()

			if deliverErr != nil {
				t.Fatalf("delivery: %v", deliverErr)
			}
			if flagErr != nil {
				t.Fatalf("flag change: %v", flagErr)
			}

			payload, errType := changesCall(t, s, "Email/changes",
				fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
			if errType != "" {
				t.Fatalf("Email/changes refused after two concurrent writes: %s", errType)
			}

			created := changedIDsOf(t, payload, "created")
			updated := changedIDsOf(t, payload, "updated")
			destroyed := changedIDsOf(t, payload, "destroyed")

			if !listHas(created, deliveredID) {
				t.Errorf("the delivered message is missing from created: created=%v updated=%v", created, updated)
			}
			// Exactly updated, not created. An earlier version of this row
			// accepted either, on the grounds that a wrong label is at least
			// visible -- and that leniency made it blind: two mutations of the
			// state description (dropping nextUID, dropping modseq) both left
			// it green, because both degrade every change into a creation.
			if !listHas(updated, existingID) {
				t.Errorf("the flag change is not reported as an update: created=%v updated=%v", created, updated)
			}
			if listHas(created, existingID) {
				t.Errorf("a message that already existed is reported as created: %v", created)
			}
			// Nothing was deleted. A false destroyed is the worst of the three:
			// a client acts on it by dropping the message from its own store.
			if len(destroyed) != 0 {
				t.Errorf("destroyed = %v after two writes that deleted nothing", destroyed)
			}
		})
	}
}

// serverSharingItsLocker builds the JMAP server and hands back the account and
// the lock service it uses, so the writers below reach the same index through
// the same coordination the server does.
func serverSharingItsLocker(t *testing.T) (*Server, string, *mailbox.UserInfo, locks.Locker) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}

	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	idx := fileindex.New(fileindex.WithLocker(locker)).OpenUser(info)

	flags := []string{`\Seen`}
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(setTestMessage), 1, int64(len(setTestMessage)), flags, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: uint32(len(setTestMessage)), VSize: vsize,
		Flags: flags, GUID: guid, InternalDate: time.Now(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("index close: %v", err)
	}

	s := New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       fileindex.New(fileindex.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
	return s, hex.EncodeToString(guid[:]), info, locker
}

// deliverAnother appends a new message, the way LMTP delivery does.
func deliverAnother(info *mailbox.UserInfo, locker locks.Locker, uid uint32) (string, error) {
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := fileindex.New(fileindex.WithLocker(locker)).OpenUser(info)
	defer idx.Close() //nolint:errcheck

	raw := fmt.Sprintf("Subject: delivered %d\r\n\r\nbody\r\n", uid)
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), uid, int64(len(raw)), nil, [16]byte{})
	if err != nil {
		return "", fmt.Errorf("save: %w", err)
	}
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		return "", fmt.Errorf("open folder: %w", err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: uid, Filename: name, Size: uint32(len(raw)), VSize: vsize, GUID: guid,
		InternalDate: time.Now(),
	}); err != nil {
		return "", fmt.Errorf("append: %w", err)
	}
	return hex.EncodeToString(guid[:]), nil
}

// flagExisting rewrites one message's flags, the way an IMAP STORE does.
func flagExisting(info *mailbox.UserInfo, locker locks.Locker, uid uint32) error {
	idx := fileindex.New(fileindex.WithLocker(locker)).OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		return fmt.Errorf("open folder: %w", err)
	}
	if err := idx.UpdateFlags(f.ID, uid, []string{`\Seen`, `\Flagged`}, nil); err != nil {
		return fmt.Errorf("update flags: %w", err)
	}
	return nil
}

func listHas(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
