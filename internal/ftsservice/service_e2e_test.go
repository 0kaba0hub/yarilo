//go:build flatcurve

package ftsservice

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/fts/buildmail"
	"github.com/yarilomail/yarilo/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const testUser = "u@test.com"

var testMbox = fts.MailboxRef{Name: "INBOX", GUID: "g-inbox", UIDValidity: 1}

func newTestService(t *testing.T) (*Service, mailbox.UserMailbox, mailbox.UserIndex) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := file.New()
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{
		Engine:      flatcurve.New(flatcurve.Options{}),
		Mailbox:     mb,
		Index:       idx,
		ResolveUser: func(u string) (*mailbox.UserInfo, error) { return resolver.UserInfo(u, ""), nil },
		Chain:       chain,
		CommitLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck

	info := resolver.UserInfo(testUser, "")
	box := mb.OpenUser(info)
	uidx := idx.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close(); uidx.Close() }) //nolint:errcheck
	return svc, box, uidx
}

func saveMessage(t *testing.T, box mailbox.UserMailbox, uidx mailbox.UserIndex, uid uint32, body string) {
	t.Helper()
	raw := "From: a@test.com\r\nSubject: note " + body + "\r\n\r\n" + body + "\r\n"
	saveRawMessage(t, box, uidx, uid, raw)
}

// saveRawMessage is saveMessage's general form, for #721's regression tests
// which need to control the exact raw bytes (a broken/malformed message, or
// one with an attachment part) rather than a plain-text body.
func saveRawMessage(t *testing.T, box mailbox.UserMailbox, uidx mailbox.UserIndex, uid uint32, raw string) {
	t.Helper()
	f, err := uidx.OpenFolder(testMbox.Name, testMbox.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	name, _, _, err := box.Save(testMbox.Name, strings.NewReader(raw), uid, int64(len(raw)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := uidx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: uid, Filename: name, Size: uint32(len(raw)),
	}); err != nil {
		t.Fatal(err)
	}
}

func waitIndexed(t *testing.T, svc ftsproto.Service, uid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		last, _, err := svc.Status(testUser, testMbox)
		if err == nil && last >= uid {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("indexing did not reach the expected checkpoint")
}

// waitChecksumReplaced blocks until the mailbox's settings checksum is no
// longer the forged one, which is what a completed rebuild leaves behind.
func waitChecksumReplaced(t *testing.T, h *userHandle, forged uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, sum, err := h.ui.Checkpoint(testMbox); err == nil && sum != forged {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the settings-drift rebuild did not replace the checkpoint checksum")
}

func lookupWord(word string) fts.Query {
	return fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{word}}}}},
		AndTerms: true,
	}
}

func TestServiceEndToEnd(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "wolves howl nightly")
	saveMessage(t, box, uidx, 2, "quiet meadow")
	saveMessage(t, box, uidx, 3, "wolves rest")

	if err := svc.Index(testUser, testMbox, 3, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 3)

	res, err := svc.Lookup(testUser, testMbox, lookupWord("wolv")) // stemmed "wolves"
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 2 || res.Definite[0] != 1 || res.Definite[1] != 3 {
		t.Fatalf("lookup = %v, want [1 3]", res.Definite)
	}

	// Expunge is synchronous.
	if err := svc.Expunge(testUser, testMbox, 1); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Lookup(testUser, testMbox, lookupWord("wolv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 3 {
		t.Fatalf("after expunge = %v, want [3]", res.Definite)
	}

	// Rescan reconciles and re-queues nothing when consistent.
	if err := svc.Rescan(testUser, testMbox); err != nil {
		t.Fatal(err)
	}
	if err := svc.Optimize(testUser); err != nil {
		t.Fatal(err)
	}
}

func TestServiceWireRoundTrip(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "remote lookup target")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go ftsproto.Serve(ln, svc) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	cl, err := ftsproto.Dial(ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.Prepend(testUser, testMbox, 1); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, cl, 1)

	res, err := cl.Lookup(testUser, testMbox, lookupWord("remot"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 1 {
		t.Fatalf("wire lookup = %v, want [1]", res.Definite)
	}
	last, sum, err := cl.Status(testUser, testMbox)
	if err != nil || last != 1 || sum == 0 {
		t.Fatalf("wire status = %d/%d/%v", last, sum, err)
	}
	if err := cl.Expunge(testUser, testMbox, 1); err != nil {
		t.Fatal(err)
	}
	if err := cl.Rescan(testUser, testMbox); err != nil {
		t.Fatal(err)
	}
	if err := cl.Optimize(testUser); err != nil {
		t.Fatal(err)
	}
}

// TestUIDValidityResetRebuild reproduces #638: a checkpoint left over from a
// mailbox that was recreated (different UIDVALIDITY) sits with a last_indexed_uid
// above the new low UIDs. Without the reset the indexer would skip every new
// message ("already current") and search would silently return nothing. The fix
// must detect the UIDVALIDITY mismatch, drop the stale index, and reindex — so a
// new low-UID message is searchable again.
func TestUIDValidityResetRebuild(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "uidvcheck")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 1)

	// Forge a stale checkpoint from a DIFFERENT UIDVALIDITY, its last_uid above the
	// mailbox's current UIDs — the exact post-recreation state that wedges search.
	h, err := svc.handle(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ui.SetCheckpoint(testMbox, 10, 999, svc.opts.Chain.SettingsChecksum()); err != nil {
		t.Fatal(err)
	}
	// Status must report "not indexed" on the UIDVALIDITY mismatch (so catch-up
	// re-triggers), not the stale last_uid=10.
	if last, _, serr := svc.Status(testUser, testMbox); serr != nil || last != 0 {
		t.Fatalf("stale-uidvalidity Status = %d/%v, want 0", last, serr)
	}

	saveMessage(t, box, uidx, 2, "uidvcheck")
	if err := svc.Index(testUser, testMbox, 2, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 2)
	res, err := svc.Lookup(testUser, testMbox, lookupWord("uidvcheck"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 2 {
		t.Fatalf("after uidvalidity reset = %v, want [1 2] (index rebuilt from scratch)", res.Definite)
	}
}

func TestSettingsDriftRebuild(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "driftcheck")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 1)

	// Forge a checkpoint with a foreign settings checksum: the next index
	// job must rebuild the mailbox and restore a working index.
	h, err := svc.handle(testUser)
	if err != nil {
		t.Fatal(err)
	}
	const foreignSum = 12345
	if err := h.ui.SetCheckpoint(testMbox, 1, testMbox.UIDValidity, foreignSum); err != nil {
		t.Fatal(err)
	}
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatal(err)
	}
	// Not waitIndexed: the forged checkpoint already reports uid 1, so a wait
	// on the last indexed uid returns before the rebuild has begun and the
	// lookup below races it. What marks the rebuild done is the checksum being
	// replaced with this build's own.
	waitChecksumReplaced(t, h, foreignSum)

	res, err := svc.Lookup(testUser, testMbox, lookupWord("driftcheck"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 {
		t.Fatalf("after drift rebuild = %v, want [1]", res.Definite)
	}
}

// countFlatcurveShards mirrors flatcurve's own on-disk shard naming
// (dbPrefix "index." / currentPrefix "current.") to verify auto-optimize
// end-to-end from outside the engine package, exactly as an operator
// inspecting the directory would.
func countFlatcurveShards(t *testing.T, dir string) (sealed, current int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch {
		case strings.HasPrefix(e.Name(), "index."):
			sealed++
		case strings.HasPrefix(e.Name(), "current."):
			current++
		}
	}
	return sealed, current
}

// TestServiceAutoOptimize (#715) is the acceptance scenario from the issue:
// index past the rotate threshold enough times to cross OptimizeLimit, and
// observe the shard count return to 1 automatically — no yarctl fts
// optimize call — while SEARCH results stay correct across the transition.
func TestServiceAutoOptimize(t *testing.T) {
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := file.New()
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{
		Engine:      flatcurve.New(flatcurve.Options{RotateCount: 2, OptimizeLimit: 3}),
		Mailbox:     mb,
		Index:       idx,
		ResolveUser: func(u string) (*mailbox.UserInfo, error) { return resolver.UserInfo(u, ""), nil },
		Chain:       chain,
		CommitLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck

	info := resolver.UserInfo(testUser, "")
	box := mb.OpenUser(info)
	uidx := idx.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close(); uidx.Close() }) //nolint:errcheck

	// RotateCount=2, OptimizeLimit=3: 6 messages seal exactly 3 shards,
	// reaching the auto-optimize threshold.
	const n = 6
	for uid := uint32(1); uid <= n; uid++ {
		saveMessage(t, box, uidx, uid, "steadyword")
	}
	if err := svc.Index(testUser, testMbox, n, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, n)

	// Keyed by the folder GUID, not the mail driver's layout (#1183).
	dir := filepath.Join(svc.indexRoot(info), testMbox.GUID, flatcurve.Label)

	// The background optimizer runs asynchronously, on its own worker
	// goroutine — it may well have already collapsed the shards back to 1
	// by the time waitIndexed returns (indexing and optimizing are
	// separate queues/workers, so there's no guaranteed ordering between
	// "checkpoint caught up" and "background optimize finished"). Poll for
	// the end state rather than asserting an in-between shard count.
	var sealed int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sealed, _ = countFlatcurveShards(t, dir); sealed == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sealed != 1 {
		t.Fatalf("auto-optimize did not collapse shards back to 1 within the deadline: sealed=%d", sealed)
	}

	res, err := svc.Lookup(testUser, testMbox, lookupWord("steadyword"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != n {
		t.Fatalf("lookup after auto-optimize = %v, want %d results", res.Definite, n)
	}
}

// hardFailDecoder always returns a genuine (non-degraded) error for a
// configured content type — simulating a permanent 4xx or a decoder
// protocol error (#697's ErrDegraded classification never applies here, so
// this is exactly the class of failure #721 is about).
type hardFailDecoder struct{ forContentType string }

func (d *hardFailDecoder) Decode(_ context.Context, contentType, _ string, body io.Reader) ([]byte, bool, error) {
	_, _ = io.Copy(io.Discard, body)
	if contentType != d.forContentType {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("hardFailDecoder: permanent failure for %s", contentType)
}

// brokenAttachmentMsg puts the failing attachment BEFORE the text/plain
// part: walkEntity aborts the whole multipart walk on the first part
// error, so only the headers ever reach up.doc before Build() fails —
// exactly the issue's "headers searchable, body absent" illustration.
const brokenAttachmentMsg = "From: a@test.com\r\n" +
	"Subject: gizmoxyzzy\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJcTl8uXrp/Og0MTGCg==\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"never reached body text\r\n" +
	"--BOUND--\r\n"

// A decoder that will not decode UID 2's attachment costs that attachment and
// nothing else: UID 2 is indexed by what could be read, UID 3 is reached, and
// the checkpoint passes all three.
//
// #721 made this halt, on the reasoning that a hard failure must not be
// skipped silently. #1219 showed what that costs: the run stopped at the same
// message on every retry, requeued every few seconds, and a query that spans
// folders turned one bad message into no search at all for the account. The
// halt is gone. Completeness is held by two other things now -- the builder
// degrades instead of refusing, so the data is always indexed, and what is
// still lost is counted and visible (fts_index_degraded_total,
// fts_index_skipped_total) rather than silent.
func TestBuildFailureCostsThePartNotTheRun(t *testing.T) {
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := file.New()
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{
		Engine:      flatcurve.New(flatcurve.Options{}),
		Mailbox:     mb,
		Index:       idx,
		ResolveUser: func(u string) (*mailbox.UserInfo, error) { return resolver.UserInfo(u, ""), nil },
		Chain:       chain,
		CommitLimit: 10,
		Build:       buildmail.Options{Decoder: &hardFailDecoder{forContentType: "application/pdf"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck

	info := resolver.UserInfo(testUser, "")
	box := mb.OpenUser(info)
	uidx := idx.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close(); uidx.Close() }) //nolint:errcheck

	saveMessage(t, box, uidx, 1, "firstmessagefine")
	saveRawMessage(t, box, uidx, 2, brokenAttachmentMsg)
	saveMessage(t, box, uidx, 3, "thirdmessagezz")

	if err := svc.Index(testUser, testMbox, 3, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 3)

	last, _, err := svc.Status(testUser, testMbox)
	if err != nil {
		t.Fatal(err)
	}
	if last != 3 {
		t.Fatalf("checkpoint = %d, want 3: one undecodable attachment stopped the run", last)
	}

	// UID 2 is findable by what could be read -- its own headers. This is the
	// difference between degrading and skipping: a skipped message is in no
	// index at all, and nothing tells a user why their mail cannot be found.
	res, err := svc.Lookup(testUser, testMbox, fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldHeader, HdrName: "subject", Words: []fts.Word{{Variants: []string{"gizmoxyzzy"}}}}},
		AndTerms: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 2 {
		t.Fatalf("lookup = %v, want [2]: the message was refused instead of indexed without its attachment", res.Definite)
	}

	// And the message after it was reached, which the halt used to prevent.
	res, err = svc.Lookup(testUser, testMbox, lookupWord("thirdmessagezz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 {
		t.Fatalf("lookup = %v, want the message after the degraded one", res.Definite)
	}
}

// A message whose MIME cannot be read is indexed by its own bytes: the words
// are there whatever the structure says, and a client looking for them finds
// the message. It halted the run under #721; #1219 showed what that costs --
// a message that will never parse turns "halt until fixed" into "never index
// this folder again", and a query spanning folders spreads it to the whole
// account. Completeness is held by the builder degrading rather than refusing,
// and by what is still lost being counted rather than silent.
func TestUnparseableMessageIsIndexedAsOpaqueText(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "firstmessagefine")
	saveRawMessage(t, box, uidx, 2, "NoColonHeaderLine\r\n\r\nbroken opaquemarkerzz body\r\n")
	saveMessage(t, box, uidx, 3, "thirdmessagezz")

	if err := svc.Index(testUser, testMbox, 3, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 3)

	last, _, err := svc.Status(testUser, testMbox)
	if err != nil {
		t.Fatal(err)
	}
	if last != 3 {
		t.Fatalf("checkpoint = %d, want 3: the unparseable message stopped the run", last)
	}

	// Found by a word from its body -- the difference between degrading and
	// skipping, and the reason the run may continue past it.
	res, err := svc.Lookup(testUser, testMbox, lookupWord("opaquemarkerzz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 2 {
		t.Fatalf("lookup = %v, want [2]: the unparseable message was not indexed at all", res.Definite)
	}

	res, err = svc.Lookup(testUser, testMbox, lookupWord("thirdmessagezz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 {
		t.Fatalf("lookup = %v, want the message after the unparseable one", res.Definite)
	}
}
