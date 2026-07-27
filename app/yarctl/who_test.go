package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestRenderWhoTable_ExampleOutput(t *testing.T) {
	body := []byte(`{
		"total": 5,
		"sessions": [
			{"id":"42","user":"alice@example.com","ip":"10.0.0.15","service":"imap","connected_at":"2026-05-31T18:14:22Z"},
			{"id":"43","user":"alice@example.com","ip":"10.0.0.15","service":"imap","connected_at":"2026-05-31T18:14:51Z"},
			{"id":"57","user":"alice@example.com","ip":"192.0.2.88","service":"submission","connected_at":"2026-05-31T18:30:01Z"},
			{"id":"51","user":"bob@example.com","ip":"203.0.113.4","service":"imap","connected_at":"2026-05-31T17:55:09Z"},
			{"id":"55","user":"bob@example.com","ip":"203.0.113.4","service":"pop3","connected_at":"2026-05-31T18:28:44Z"}
		]
	}`)
	var buf bytes.Buffer
	if err := renderWhoTable(&buf, body, false); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("=== who (table) ===\n%s\n", buf.String())
}

func TestRenderWhoTable_Empty(t *testing.T) {
	body := []byte(`{"total":0,"sessions":[]}`)
	var buf bytes.Buffer
	if err := renderWhoTable(&buf, body, false); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("=== who (table, empty) ===\n%s\n", buf.String())
}

func TestRenderCountTable_Plain(t *testing.T) {
	body := []byte(`{"total":5,"service":"","user":""}`)
	var buf bytes.Buffer
	if err := renderCountTable(&buf, body, ""); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("=== who count ===\n%s\n", buf.String())
}

func TestRenderCountTable_ImapOnly(t *testing.T) {
	body := []byte(`{"total":3,"service":"imap","user":""}`)
	var buf bytes.Buffer
	if err := renderCountTable(&buf, body, ""); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("=== who count imap ===\n%s\n", buf.String())
}

func TestRenderCountTable_ByProtocol(t *testing.T) {
	body := []byte(`{"total":5,"by_protocol":{"imap":3,"pop3":1,"submission":1}}`)
	var buf bytes.Buffer
	if err := renderCountTable(&buf, body, "protocol"); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("=== who count --by protocol ===\n%s\n", buf.String())
}

func TestRenderCountTable_ByUser(t *testing.T) {
	body := []byte(`{"total":5,"by_user":{"alice@example.com":3,"bob@example.com":2}}`)
	var buf bytes.Buffer
	if err := renderCountTable(&buf, body, "user"); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("=== who count --by user ===\n%s\n", buf.String())
}

func TestRenderWhoTable_AllShowsBackendColumn(t *testing.T) {
	body := []byte(`{"total":1,"sessions":[
		{"id":"1","user":"a@d.test","ip":"1.2.3.4","service":"imap","connected_at":"2026-05-31T18:00:00Z","backend":"10.0.0.7"}
	]}`)
	var buf bytes.Buffer
	if err := renderWhoTable(&buf, body, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "BACKEND") || !strings.Contains(out, "10.0.0.7") {
		t.Fatalf("--all view must show BACKEND column + value, got:\n%s", out)
	}
	// Default (no --all) omits it.
	var buf2 bytes.Buffer
	if err := renderWhoTable(&buf2, body, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), "BACKEND") {
		t.Fatalf("default view must NOT show BACKEND column, got:\n%s", buf2.String())
	}
}
