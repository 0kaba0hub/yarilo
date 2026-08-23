package threads

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Writers are serialised across processes by the account's thread lock. What
// that lock does not cover is one process reading these maps while a delivery
// applies a placement to them -- and the distributed lock is the wrong tool
// for it: a reader would pay a round trip to the lock service per request and
// queue behind LMTP.
//
// Run with -race. Without the RWMutex this is a data race, and the detector is
// the assertion.
func TestReadersAndDeliveriesShareTheStateSafely(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	// ONE cache, shared by the writer and the reader, because that is the
	// deployment: a backend process runs LMTP delivery and JMAP reads against
	// the same fold. An earlier version of this test gave the reader its own
	// cache -- separate states, no shared object, and the detector had nothing
	// to find. It passed with the lock removed.
	cache := NewCache(time.Minute)
	rec := NewRecorder(cache)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// A reader, doing what Thread/get does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			state, err := cache.Get("u@example.com", path)
			if err != nil {
				t.Error(err)
				return
			}
			state.Read(func(v View) {
				for _, id := range v.Threads() {
					_ = v.Members(id)
				}
			})
		}
	}()

	for i := 0; i < 200; i++ {
		raw := fmt.Sprintf("Message-ID: <m%d@x>\r\nSubject: Plan %d\r\n\r\nbody\r\n", i, i%5)
		if _, err := rec.Record("u@example.com", path, fmt.Sprintf("guid-%d", i), []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// A merge writes a placement and an alias together. A reader that caught the
// state between them would see a message in a conversation whose alias had not
// been applied -- an answer assembled from two states, describing a
// conversation that never existed.
//
// The memory half of Append is one critical section, so there is no between.
func TestAReaderNeverSeesAPlacementWithoutItsMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	state, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Two conversations, about to be joined.
	mustAppend(t, path, state, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "g1"})
	mustAppend(t, path, state, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "g2"})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			state.Read(func(v View) {
				// Whatever the writer is doing, these two answers agree: the
				// thread a message reports is a thread that lists it.
				for _, guid := range []string{"g1", "g2", "g3"} {
					id, ok := v.ThreadOf(guid)
					if !ok {
						continue
					}
					var listed bool
					for _, m := range v.Members(id) {
						if m == guid {
							listed = true
						}
					}
					if !listed {
						t.Errorf("%s reports thread %q, which does not list it -- the read caught a half-applied merge", guid, id)
						return
					}
				}
			})
		}
	}()

	for i := 0; i < 100; i++ {
		st, lerr := Load(path)
		if lerr != nil {
			t.Fatal(lerr)
		}
		p := Placement{
			GUID: fmt.Sprintf("g%d", i+3), MessageID: fmt.Sprintf("late%d@x", i),
			ThreadID: "g1", MergedFrom: []string{"g2"},
		}
		if aerr := Append(path, state, p); aerr != nil {
			t.Fatal(aerr)
		}
		_ = st
	}
	close(stop)
	wg.Wait()
}
