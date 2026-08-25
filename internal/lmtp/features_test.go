package lmtp

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type featureServer struct {
	addr       string
	maildirCur string // alice's INBOX/cur path for direct file inspection
}

func buildFeatureServer(t *testing.T, cfg config.LMTPProtocolConfig) featureServer {
	t.Helper()
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := fileindex.New()

	box := mb.OpenUser(resolver.UserInfo("alice@example.com", ""))
	if err := box.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	box.Close() //nolint:errcheck

	srv := New(Options{Hostname: "lmtp.test", Config: cfg, Mailbox: mb, Index: idx, Resolver: resolver})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	return featureServer{
		addr:       ln.Addr().String(),
		maildirCur: filepath.Join(resolver.Resolve("alice@example.com", ""), "Maildir", "cur"),
	}
}

func TestLMTP_HdrDeliveryAddress_Final(t *testing.T) {
	fs := buildFeatureServer(t, config.LMTPProtocolConfig{
		ReadTimeout:        5,
		WriteTimeout:       5,
		HdrDeliveryAddress: "final",
	})
	conn, sc := dialLMTP(t, fs.addr)
	sendLHLO(t, conn, sc)
	resp := deliver(t, conn, sc, "sender@external.com", "alice+tag@example.com", testMsg)
	if !strings.HasPrefix(resp[0], "250") {
		t.Fatalf("expected 250, got: %q", resp[0])
	}
	// final: detail stripped → alice@example.com
	checkDirHeader(t, fs.maildirCur, "Delivered-To", "alice@example.com")
}

func TestLMTP_HdrDeliveryAddress_Original(t *testing.T) {
	fs := buildFeatureServer(t, config.LMTPProtocolConfig{
		ReadTimeout:        5,
		WriteTimeout:       5,
		HdrDeliveryAddress: "original",
	})
	conn, sc := dialLMTP(t, fs.addr)
	sendLHLO(t, conn, sc)
	resp := deliver(t, conn, sc, "sender@external.com", "alice+tag@example.com", testMsg)
	if !strings.HasPrefix(resp[0], "250") {
		t.Fatalf("expected 250, got: %q", resp[0])
	}
	// original: +tag kept
	checkDirHeader(t, fs.maildirCur, "Delivered-To", "alice+tag@example.com")
}

func TestLMTP_HdrDeliveryAddress_None(t *testing.T) {
	fs := buildFeatureServer(t, config.LMTPProtocolConfig{
		ReadTimeout:        5,
		WriteTimeout:       5,
		HdrDeliveryAddress: "none",
	})
	conn, sc := dialLMTP(t, fs.addr)
	sendLHLO(t, conn, sc)
	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if !strings.HasPrefix(resp[0], "250") {
		t.Fatalf("expected 250, got: %q", resp[0])
	}
	checkDirNoHeader(t, fs.maildirCur, "Delivered-To")
}

func TestParseWorkarounds(t *testing.T) {
	cases := []struct {
		input []string
		want  lmtpWorkarounds
	}{
		{nil, 0},
		{[]string{"whitespace-before-path"}, workaroundWhitespaceBeforePath},
		{[]string{"mailbox-for-path"}, workaroundMailboxForPath},
		{[]string{"whitespace-before-path", "mailbox-for-path"}, workaroundWhitespaceBeforePath | workaroundMailboxForPath},
		{[]string{"unknown-thing"}, 0},
	}
	for _, tc := range cases {
		got, _ := parseWorkarounds(tc.input)
		if got != tc.want {
			t.Errorf("parseWorkarounds(%v) = %b, want %b", tc.input, got, tc.want)
		}
	}
}

// checkDirHeader reads the first file in curDir and asserts header is present with value.
func checkDirHeader(t *testing.T, curDir, header, value string) {
	t.Helper()
	f := firstFileIn(t, curDir)
	checkFileHeader(t, f, header, value)
}

// checkDirNoHeader asserts that header is NOT present in any message in curDir.
func checkDirNoHeader(t *testing.T, curDir, header string) {
	t.Helper()
	f := firstFileIn(t, curDir)
	checkFileNoHeader(t, f, header)
}

func firstFileIn(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			return filepath.Join(dir, e.Name())
		}
	}
	t.Fatalf("no files in %s", dir)
	return ""
}

func checkFileHeader(t *testing.T, path, header, value string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	prefix := strings.ToLower(header) + ":"
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimRight(line, "\r") == "" {
			break // end of headers
		}
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			if strings.Contains(line, value) {
				return
			}
			t.Fatalf("header %q found but value %q missing: %q", header, value, line)
		}
	}
	t.Fatalf("header %q not found in %s", header, path)
}

func checkFileNoHeader(t *testing.T, path, header string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	prefix := strings.ToLower(header) + ":"
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimRight(line, "\r") == "" {
			return // end of headers, not found — good
		}
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			t.Fatalf("unexpected header %q in %s: %q", header, path, line)
		}
	}
}
func TestLMTP_QuotaEnforcement_452(t *testing.T) {
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := fileindex.New()

	box := mb.OpenUser(resolver.UserInfo("alice@example.com", ""))
	if err := box.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	box.Close() //nolint:errcheck

	// Quota comes from the index (count backend). A tiny 10-byte limit means the
	// incoming test message alone exceeds it → 452.
	srv := New(Options{
		Hostname:    "lmtp.test",
		Config:      config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:     mb,
		Index:       idx,
		QuotaEngine: true,
		Resolver: &mailbox.Resolver{
			Root:              dir,
			HomeTemplate:      "%d/%n",
			DefaultQuotaRules: []string{"*:storage=10"},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)
	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if len(resp) == 0 || !strings.HasPrefix(resp[0], "452") {
		t.Fatalf("expected 452 Mailbox full, got: %v", resp)
	}
}

// The warning names what the operator could have meant, so the advertised set
// has to be the accepted set: a stale list here answers a wrong name with
// another wrong name.
func TestKnownWorkaroundsMatchesTheParser(t *testing.T) {
	for _, name := range knownWorkarounds() {
		mask, unknown := parseWorkarounds([]string{name})
		if mask == 0 || len(unknown) != 0 {
			t.Errorf("knownWorkarounds() offers %q, which the parser does not accept", name)
		}
	}
}
