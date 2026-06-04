package quotastatus_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/quotastatus"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/dict/memory"
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

func TestPolicyCheck_UnderQuota(t *testing.T) {
	d := newMemDict(t)
	lim := quota.ParseRules([]string{"*:storage=10M"})
	addr := startServer(t, quotastatus.Options{QuotaDict: d, Limits: lim})

	// User has used 1 byte — well under 10M.
	ctr := quota.NewCounter(d, "alice@example.com")
	_ = ctr.Add(context.Background(), 1, 1)

	action := policyCheck(t, addr, map[string]string{
		"request":   "smtpd_access_policy",
		"recipient": "alice@example.com",
		"size":      "1024",
	})
	if action != "DUNNO" {
		t.Errorf("want DUNNO, got %q", action)
	}
}

func TestPolicyCheck_OverQuota(t *testing.T) {
	d := newMemDict(t)
	lim := quota.ParseRules([]string{"*:storage=1K"})
	addr := startServer(t, quotastatus.Options{QuotaDict: d, Limits: lim})

	// Pre-fill to exactly the limit.
	ctr := quota.NewCounter(d, "alice@example.com")
	_ = ctr.Add(context.Background(), 1024, 5)

	action := policyCheck(t, addr, map[string]string{
		"request":   "smtpd_access_policy",
		"recipient": "alice@example.com",
		"size":      "100",
	})
	if !strings.HasPrefix(action, "REJECT 452") {
		t.Errorf("want REJECT 452, got %q", action)
	}
}

func TestPolicyCheck_IgnoreFolder(t *testing.T) {
	d := newMemDict(t)
	lim := quota.ParseRules([]string{"*:storage=1K", "Spam:ignore"})
	addr := startServer(t, quotastatus.Options{QuotaDict: d, Limits: lim})

	// Pre-fill past the limit.
	ctr := quota.NewCounter(d, "alice@example.com")
	_ = ctr.Add(context.Background(), 2048, 10)

	// Delivery to Spam folder via detail address — must be allowed.
	action := policyCheck(t, addr, map[string]string{
		"request":   "smtpd_access_policy",
		"recipient": "alice+Spam@example.com",
		"size":      "100",
	})
	if action != "DUNNO" {
		t.Errorf("want DUNNO for ignored folder, got %q", action)
	}
}

func TestPolicyCheck_NoQuotaDict(t *testing.T) {
	addr := startServer(t, quotastatus.Options{
		QuotaDict: nil,
		Limits:    quota.ParseRules([]string{"*:storage=1K"}),
	})

	action := policyCheck(t, addr, map[string]string{
		"request":   "smtpd_access_policy",
		"recipient": "alice@example.com",
		"size":      "9999",
	})
	if action != "DUNNO" {
		t.Errorf("want DUNNO when no dict, got %q", action)
	}
}

func TestPolicyCheck_MultipleRequestsPerConn(t *testing.T) {
	d := newMemDict(t)
	lim := quota.ParseRules([]string{"*:storage=10M"})
	addr := startServer(t, quotastatus.Options{QuotaDict: d, Limits: lim})

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
