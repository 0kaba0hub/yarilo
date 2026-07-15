package quotastatus_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/quotastatus"
	file "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/dict/memory"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

func newMemDict(t *testing.T) dict.Dict {
	t.Helper()
	d, err := memory.New(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("memory dict: %v", err)
	}
	return d
}

func startServer(t *testing.T, opts quotastatus.Options) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := quotastatus.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }() //nolint:errcheck
	return ln.Addr().String()
}

func policyCheck(t *testing.T, addr string, attrs map[string]string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var sb strings.Builder
	for k, v := range attrs {
		fmt.Fprintf(&sb, "%s=%s\n", k, v)
	}
	sb.WriteString("\n")
	if _, err := fmt.Fprint(conn, sb.String()); err != nil {
		t.Fatalf("write request: %v", err)
	}

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "action=") {
			return strings.TrimPrefix(line, "action=")
		}
	}
	t.Fatal("no action in response")
	return ""
}

// startStorageServer builds a quota-status server backed by real maildir+index
// storage: each user in userBytes gets one INBOX message of that virtual size,
// and UserdbLookup returns quotaRules. This mirrors production, where the count
// backend sums the recipient's index.
func startStorageServer(t *testing.T, quotaRules []string, aliasD dict.Dict, hops int, userBytes map[string]uint32) string {
	return startStorageServerOpts(t, quotaRules, aliasD, hops, userBytes, nil)
}

// startStorageServerOpts is startStorageServer with a hook to tweak the final
// Options (recipient_delimiter, mail_size, exceeded_message, ...).
func startStorageServerOpts(t *testing.T, quotaRules []string, aliasD dict.Dict, hops int, userBytes map[string]uint32, mutate func(*quotastatus.Options)) string {
	t.Helper()
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := file.New()
	for user, b := range userBytes {
		ui := resolver.UserInfo(user, "")
		box := mb.OpenUser(ui)
		_ = box.Init()
		box.Close() //nolint:errcheck
		uidx := idx.OpenUser(ui)
		f, err := uidx.OpenFolder("INBOX", 1)
		if err != nil {
			t.Fatalf("open INBOX for %s: %v", user, err)
		}
		if b > 0 {
			if err := uidx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, VSize: b, Size: b}); err != nil {
				t.Fatalf("append for %s: %v", user, err)
			}
		}
		uidx.Close() //nolint:errcheck
	}
	lookup := func(_ context.Context, username string) (*mailbox.UserInfo, error) {
		mi := resolver.UserInfo(username, "")
		mi.QuotaRules = quotaRules
		return mi, nil
	}
	opts := quotastatus.Options{
		Enabled:      true,
		Limits:       quota.ParseRules(quotaRules),
		UserdbLookup: lookup,
		Mailbox:      mb,
		Index:        idx,
		AliasDict:    aliasD,
		AliasMaxHops: hops,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return startServer(t, opts)
}

func TestPolicyCheck_RecipientDelimiter(t *testing.T) {
	// With delimiter "-", the "-Spam" detail selects the ignored folder so an
	// otherwise-over-quota user is allowed; "+" is treated as a literal.
	addr := startStorageServerOpts(t, []string{"*:storage=1K", "Spam:ignore"}, nil, 0,
		map[string]uint32{"alice@example.com": 2048},
		func(o *quotastatus.Options) { o.RecipientDelimiter = "-" })
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice-Spam@example.com", "size": "100",
	}); a != "DUNNO" {
		t.Errorf("want DUNNO for delimiter-ignored folder, got %q", a)
	}
	// "+" is no longer a delimiter, so alice+Spam maps to a different username.
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice+Spam@example.com", "size": "100",
	}); !strings.HasPrefix(a, "DUNNO") && !strings.HasPrefix(a, "REJECT") {
		t.Errorf("unexpected action %q", a)
	}
}

func TestPolicyCheck_MailSize(t *testing.T) {
	addr := startStorageServerOpts(t, []string{"*:storage=10M"}, nil, 0,
		map[string]uint32{"alice@example.com": 1},
		func(o *quotastatus.Options) { o.MailSize = 500 })
	// Under the cap → allowed.
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
	}); a != "DUNNO" {
		t.Errorf("want DUNNO under mail_size, got %q", a)
	}
	// Over the cap → distinct 552 rejection.
	a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "600",
	})
	if !strings.HasPrefix(a, "REJECT 552") || !strings.Contains(a, "max mail size") {
		t.Errorf("want REJECT 552 max mail size, got %q", a)
	}
}

func TestPolicyCheck_ExceededMessage(t *testing.T) {
	addr := startStorageServerOpts(t, []string{"*:storage=1K"}, nil, 0,
		map[string]uint32{"alice@example.com": 1024},
		func(o *quotastatus.Options) { o.ExceededMessage = "over the top" })
	a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
	})
	if !strings.HasPrefix(a, "REJECT 452") || !strings.Contains(a, "over the top") {
		t.Errorf("want custom exceeded message, got %q", a)
	}
}

func TestPolicyCheck_UnderQuota(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=10M"}, nil, 0, map[string]uint32{"alice@example.com": 1})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "1024",
	})
	if action != "DUNNO" {
		t.Errorf("want DUNNO, got %q", action)
	}
}

func TestPolicyCheck_OverQuota(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=1K"}, nil, 0, map[string]uint32{"alice@example.com": 1024})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "REJECT 452") {
		t.Errorf("want REJECT 452, got %q", action)
	}
}

func TestPolicyCheck_IgnoreFolder(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=1K", "Spam:ignore"}, nil, 0, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice+Spam@example.com", "size": "100",
	})
	if action != "DUNNO" {
		t.Errorf("want DUNNO for ignored folder, got %q", action)
	}
}

func TestPolicyCheck_NoStorage(t *testing.T) {
	// No Mailbox/Index/UserdbLookup wired → fail-open.
	addr := startServer(t, quotastatus.Options{Limits: quota.ParseRules([]string{"*:storage=1K"})})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "9999",
	})
	if action != "DUNNO" {
		t.Errorf("want DUNNO when no storage, got %q", action)
	}
}

func setAlias(t *testing.T, d dict.Dict, src, dst string) {
	t.Helper()
	tx, err := d.Begin(context.Background(), &dict.OpSettings{})
	if err != nil {
		t.Fatalf("alias begin: %v", err)
	}
	if err := tx.Set(src, []byte(dst)); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("alias commit: %v", err)
	}
}

func TestAliasResolution_DirectAlias(t *testing.T) {
	aliasD := newMemDict(t)
	setAlias(t, aliasD, "info@example.com", "alice@example.com")
	addr := startStorageServer(t, []string{"*:storage=1K"}, aliasD, 0, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "info@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "REJECT 452") {
		t.Errorf("alias resolved to over-quota alice: want REJECT 452, got %q", action)
	}
}

func TestAliasResolution_ChainedAlias(t *testing.T) {
	aliasD := newMemDict(t)
	setAlias(t, aliasD, "sales@example.com", "info@example.com")
	setAlias(t, aliasD, "info@example.com", "alice@example.com")
	addr := startStorageServer(t, []string{"*:storage=1K"}, aliasD, 5, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "sales@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "REJECT 452") {
		t.Errorf("chained alias: want REJECT 452, got %q", action)
	}
}

func TestAliasResolution_DetailStrippedForLookup(t *testing.T) {
	aliasD := newMemDict(t)
	setAlias(t, aliasD, "info@example.com", "alice@example.com")
	addr := startStorageServer(t, []string{"*:storage=1K"}, aliasD, 0, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "info+newsletter@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "REJECT 452") {
		t.Errorf("detail+alias: want REJECT 452, got %q", action)
	}
}

func TestAliasResolution_NoAlias_FallsBackToDirect(t *testing.T) {
	aliasD := newMemDict(t)
	addr := startStorageServer(t, []string{"*:storage=10M"}, aliasD, 0, map[string]uint32{"bob@example.com": 0})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "bob@example.com", "size": "100",
	})
	if action != "DUNNO" {
		t.Errorf("no alias, under quota: want DUNNO, got %q", action)
	}
}

func TestPolicyCheck_MultipleRequestsPerConn(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=10M"}, nil, 0, map[string]uint32{"bob@example.com": 0})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for i := 0; i < 3; i++ {
		fmt.Fprintf(conn, "request=smtpd_access_policy\nrecipient=bob@example.com\nsize=100\n\n") //nolint:errcheck
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "action=") {
				if line != "action=DUNNO" {
					t.Errorf("request %d: want action=DUNNO, got %q", i, line)
				}
				break
			}
		}
	}
}
