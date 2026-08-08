package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A corrupted cache header is enough to reach the failure the lock comment
// describes, with no concurrency at all: the file is discarded and recreated,
// and if that reuses the generation, the stamps still in the index apply to
// whatever lands at those offsets next. The first message then reads back the
// second one's envelope -- indexid matches, file_seq matches, the record
// decodes, so no invalidation level can see it (#1184).
func TestCacheRecreateEntersANewGeneration(t *testing.T) {
	root, addr := startEnvelopeCacheServer(t)
	c := dialRaw(t, addr)
	c.login()
	appendSubject(t, c, "INBOX", "first-message")
	c.cmd(`SELECT INBOX`)
	if out := c.cmd(`FETCH 1 (ENVELOPE)`); !strings.Contains(out, "first-message") {
		t.Fatalf("first message not cached:\n%s", out)
	}

	var cachePath string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && filepath.Base(p) == "yarilo.index.cache" {
			cachePath = p
		}
		return nil
	})
	if cachePath == "" {
		t.Fatal("no cache file after the first fetch")
	}
	// A torn header: the file is discarded on the next open.
	if err := os.WriteFile(cachePath, []byte("torn"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A second message now fills the fresh file from its start -- the offsets
	// the first message's stamp still names.
	appendSubject(t, c, "INBOX", "second-message")
	c.cmd(`SELECT INBOX`)
	if out := c.cmd(`FETCH 2 (ENVELOPE)`); !strings.Contains(out, "second-message") {
		t.Fatalf("second message not cached:\n%s", out)
	}

	out := c.cmd(`FETCH 1 (ENVELOPE)`)
	if strings.Contains(out, "second-message") {
		t.Errorf("message 1 answered with message 2's envelope -- the recreated cache reused "+
			"the generation, so the stale stamp resolved into it:\n%s", out)
	}
	if !strings.Contains(out, "first-message") {
		t.Errorf("message 1 lost its own envelope:\n%s", out)
	}
}
