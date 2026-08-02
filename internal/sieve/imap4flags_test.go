package sieve

import (
	"context"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

func TestImap4flagsAddflagKeep(t *testing.T) {
	e := New(config.SieveConfig{
		Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536, DefaultName: FallbackDefaultName,
	}, nil, nil, nil)
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	script := "require \"imap4flags\";\naddflag \"\\\\Flagged\";\nkeep;\n"
	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	result, err := e.Filter(ctx, baseOpts("u1", homeDir))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	t.Logf("Deliveries: %+v", result.Deliveries)
	if len(result.Deliveries) == 0 {
		t.Fatal("no deliveries")
	}
	d := result.Deliveries[0]
	if d.Folder != "INBOX" {
		t.Errorf("expected INBOX, got %q", d.Folder)
	}
	found := false
	for _, f := range d.Flags {
		if f == `\Flagged` {
			found = true
		}
	}
	if !found {
		t.Errorf("\\Flagged not in flags: %v", d.Flags)
	}
}
