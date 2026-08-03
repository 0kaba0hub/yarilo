package ftsservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// fakeBox serves message bodies and can fail a chosen one, which is how the
// error-attribution property is exercised.
type fakeBox struct {
	mailbox.UserMailbox
	bodies  map[string]string
	failOn  string
	opened  atomic.Int32
	release chan struct{}
}

func (f *fakeBox) Fetch(_, name string, _ bool) (io.ReadCloser, error) {
	f.opened.Add(1)
	if name == f.failOn {
		return nil, errors.New("storage: message vanished")
	}
	if f.release != nil {
		<-f.release
	}
	body, ok := f.bodies[name]
	if !ok {
		return nil, fmt.Errorf("no such message %q", name)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func metas(n int, size uint32) []*mailbox.MessageMeta {
	out := make([]*mailbox.MessageMeta, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, &mailbox.MessageMeta{
			UID: uint32(i), Filename: fmt.Sprintf("m%d", i), Size: size,
		})
	}
	return out
}

func bodies(n int, size int) map[string]string {
	out := make(map[string]string, n)
	for i := 1; i <= n; i++ {
		out[fmt.Sprintf("m%d", i)] = strings.Repeat("x", size)
	}
	return out
}

// Order is the invariant the checkpoint rests on: indexing walks UIDs upwards
// and records progress as it goes, so a pipeline that delivered out of order
// would record progress past messages it never indexed.
func TestFetcherPreservesUIDOrder(t *testing.T) {
	box := &fakeBox{bodies: bodies(50, 64)}
	h := &userHandle{box: box}
	f := newFetcher(h, "INBOX", prefetchOptions{Depth: 8, MaxBytes: 1 << 20})

	var got []uint32
	for item := range f.run(context.Background(), metas(50, 64)) {
		got = append(got, item.meta.UID)
		f.release(int64(item.meta.Size))
	}
	if len(got) != 50 {
		t.Fatalf("got %d messages, want 50", len(got))
	}
	for i, uid := range got {
		if uid != uint32(i+1) {
			t.Fatalf("message %d has uid %d — the pipeline reordered", i, uid)
		}
	}
}

// A read failure must arrive attached to the message it belongs to. Losing that
// association is the one way this change could corrupt indexing progress: the
// caller skips a uid it can name, and halts on nothing it cannot.
func TestFetcherAttributesAReadFailureToItsMessage(t *testing.T) {
	box := &fakeBox{bodies: bodies(5, 64), failOn: "m3"}
	h := &userHandle{box: box}
	f := newFetcher(h, "INBOX", prefetchOptions{Depth: 4, MaxBytes: 1 << 20})

	seen := map[uint32]error{}
	for item := range f.run(context.Background(), metas(5, 64)) {
		seen[item.meta.UID] = item.err
		f.release(int64(item.meta.Size))
	}
	if len(seen) != 5 {
		t.Fatalf("got %d messages, want all 5 — a failure must not truncate the pass", len(seen))
	}
	if seen[3] == nil {
		t.Error("uid 3 failed to read but arrived without an error")
	}
	for _, uid := range []uint32{1, 2, 4, 5} {
		if seen[uid] != nil {
			t.Errorf("uid %d carries an error that belongs to another message: %v", uid, seen[uid])
		}
	}
}

// The byte ceiling is what stops a few large attachments sitting in memory
// together — depth alone does not bound anything.
func TestFetcherRespectsTheByteCeiling(t *testing.T) {
	const size = 1 << 20 // 1 MiB each
	box := &fakeBox{bodies: bodies(8, size)}
	h := &userHandle{box: box}
	// Depth would allow 8 in flight; the ceiling allows 2.
	f := newFetcher(h, "INBOX", prefetchOptions{Depth: 8, MaxBytes: 2 * size})

	src := f.run(context.Background(), metas(8, size))
	// Let the producer run ahead as far as it can before consuming anything.
	time.Sleep(150 * time.Millisecond)
	if opened := box.opened.Load(); opened > 3 {
		t.Errorf("opened %d messages with a 2-message ceiling — the window is not bounding anything", opened)
	}
	for item := range src {
		f.release(int64(item.meta.Size))
	}
}

// A message larger than the whole ceiling must still be indexed. Refusing it
// would be worse than briefly exceeding the bound the ceiling expresses.
func TestFetcherAdmitsAMessageLargerThanTheCeiling(t *testing.T) {
	const size = 4 << 20
	box := &fakeBox{bodies: bodies(2, size)}
	h := &userHandle{box: box}
	f := newFetcher(h, "INBOX", prefetchOptions{Depth: 4, MaxBytes: 1 << 20})

	count := 0
	for item := range f.run(context.Background(), metas(2, size)) {
		if item.err != nil {
			t.Fatalf("uid %d: %v", item.meta.UID, item.err)
		}
		count++
		f.release(int64(item.meta.Size))
	}
	if count != 2 {
		t.Errorf("indexed %d of 2 oversized messages", count)
	}
}

// Cancelling must release the producer rather than leaving it parked on a
// window nobody will drain.
func TestFetcherStopsOnCancel(t *testing.T) {
	const size = 1 << 20
	box := &fakeBox{bodies: bodies(20, size), release: make(chan struct{})}
	h := &userHandle{box: box}
	f := newFetcher(h, "INBOX", prefetchOptions{Depth: 2, MaxBytes: size})

	ctx, cancel := context.WithCancel(context.Background())
	src := f.run(ctx, metas(20, size))
	cancel()
	close(box.release)

	done := make(chan struct{})
	go func() {
		for range src { //nolint:revive // draining is the point
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the producer did not stop after cancel")
	}
}

// Depth below two is the sequential path, kept as one code path rather than a
// branch on configuration.
func TestPrefetchDisabledBelowDepthTwo(t *testing.T) {
	tests := []struct {
		name string
		opts prefetchOptions
		want bool
	}{
		{"default", prefetchOptions{Depth: 4, MaxBytes: 32 << 20}, true},
		{"depth one", prefetchOptions{Depth: 1, MaxBytes: 32 << 20}, false},
		{"depth zero", prefetchOptions{Depth: 0, MaxBytes: 32 << 20}, false},
		{"no ceiling", prefetchOptions{Depth: 4}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.enabled(); got != tt.want {
				t.Errorf("enabled = %t, want %t", got, tt.want)
			}
		})
	}
}

// Depth one must still deliver every message, in order: disabling prefetch
// cannot mean disabling indexing.
func TestFetcherWorksWithPrefetchDisabled(t *testing.T) {
	box := &fakeBox{bodies: bodies(6, 128)}
	h := &userHandle{box: box}
	f := newFetcher(h, "INBOX", prefetchOptions{Depth: 1, MaxBytes: 1 << 20})

	var got []uint32
	for item := range f.run(context.Background(), metas(6, 128)) {
		if item.err != nil {
			t.Fatalf("uid %d: %v", item.meta.UID, item.err)
		}
		got = append(got, item.meta.UID)
		f.release(int64(item.meta.Size))
	}
	if len(got) != 6 {
		t.Fatalf("got %d messages, want 6", len(got))
	}
	for i, uid := range got {
		if uid != uint32(i+1) {
			t.Fatalf("out of order at %d: %d", i, uid)
		}
	}
}
