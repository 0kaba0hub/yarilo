package imap_test

import (
	"net"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/prometheus/client_golang/prometheus/testutil"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/quotawarn"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// startEnforcingServer is the quota-warning harness with save-time enforcement
// actually on -- the other one leaves QuotaEngine unset, so checkQuota returns
// before it counts anything and a test of the counting measures nothing.
func startEnforcingServer(t *testing.T, dir string) *imapclient.Client {
	t.Helper()
	opts := imapserver.Options{
		Mailbox:     maildir.New(),
		Index:       file.New(),
		Resolver:    &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		Auth:        &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=1000"},
		QuotaEngine: true,
		QuotaPolicy: quota.Policy{
			Warnings: []quota.Warning{
				{Name: "under90", Resource: "storage", Threshold: "under", Percentage: 90},
			},
		},
		QuotaWarner: quotawarn.New("", 5),
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

func walks(reason string) float64 {
	return testutil.ToFloat64(quota.MetricUsageCount.WithLabelValues("miss", reason))
}

func hits(reason string) float64 {
	return testutil.ToFloat64(quota.MetricUsageCount.WithLabelValues("hit", reason))
}

// One save walks the user's folders once, and the warning baseline reads what
// that walk left rather than making its own.
//
// Both walked, one after the other, on paths that run for the same command --
// and a walk opens every folder and takes a lock on each (#1634). The reference
// counts once per transaction and does arithmetic inside it (quota.c:736).
//
// The baseline is exercised through SELECT, which is where it is seeded: an
// APPEND alone never calls it, so a test built only on APPEND asserts nothing
// about it -- the first version of this one passed with the fix removed.
func TestOneSaveWalksTheFoldersOnce(t *testing.T) {
	dir := t.TempDir()
	c := startEnforcingServer(t, dir)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	beforeEnforce := walks("enforce")

	body := []byte("From: a@b.test\r\nSubject: one\r\n\r\nbody\r\n")
	ac := c.Append("INBOX", int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	// Close only ends the literal. Without waiting for the tagged response the
	// counters are read before the server has run the command at all -- which
	// is how the first version of this test read zero for everything.
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("append: %v", err)
	}

	if enforce := walks("enforce") - beforeEnforce; enforce != 1 {
		t.Errorf("enforcement walked the folders %v times for one save, want 1", enforce)
	}

	// Now the baseline, on the path that actually takes it -- an expunge, which
	// captures the "before" side unconditionally -- with enforcement's value
	// still fresh. SELECT will not do: it seeds only when no snapshot exists,
	// and the append just made one.
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := c.Store(imap.SeqSetNum(1), store, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
	beforeMiss := walks("warning-baseline")
	beforeHit := hits("warning-baseline")
	if err := c.Expunge().Close(); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	if miss := walks("warning-baseline") - beforeMiss; miss != 0 {
		t.Errorf("the warning baseline walked the folders %v times, want 0", miss)
	}
	if hit := hits("warning-baseline") - beforeHit; hit != 1 {
		t.Errorf("the warning baseline read the counted value %v times, want 1", hit)
	}
}

// And the decision itself is unchanged: a message over the limit is still
// refused. The saving must not buy speed with a wrong answer at the boundary.
func TestASaveOverTheLimitIsStillRefused(t *testing.T) {
	dir := t.TempDir()
	c := startEnforcingServer(t, dir)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	// The stub grants bytes=1000; this one message is past it on its own.
	body := []byte("From: a@b.test\r\nSubject: big\r\n\r\n" + strings.Repeat("x", 2000) + "\r\n")
	ac := c.Append("INBOX", int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("closing the literal: %v", err)
	}
	if _, err := ac.Wait(); err == nil {
		t.Error("a message past the limit was accepted")
	}
}
