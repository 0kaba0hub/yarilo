package spf

import (
	"context"
	"net"
	"testing"
)

var extractDomainCases = []struct {
	addr    string
	want    string
	wantErr bool
}{
	{"user@example.com", "example.com", false},
	{"<user@example.com>", "example.com", false},
	{"<>", "", false},     // null sender (bounces)
	{"", "", false},       // null sender
	{"nodomain", "", true},
}

func TestExtractDomain(t *testing.T) {
	for _, tc := range extractDomainCases {
		tc := tc
		t.Run(tc.addr, func(t *testing.T) {
			got, err := extractDomain(tc.addr)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got domain %q", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("extractDomain(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestCheck_MalformedSender verifies that a malformed sender returns PermError.
func TestCheck_MalformedSender(t *testing.T) {
	ip := net.ParseIP("1.2.3.4")
	result, err := Check(context.Background(), ip, "notanemail", "")
	if err == nil {
		t.Errorf("expected error for malformed sender, got result %q", result)
	}
	if result != PermError {
		t.Errorf("expected PermError for malformed sender, got %q", result)
	}
}

// TestCheck_NullSender verifies that a null sender (<>) does not error on domain extraction.
// The SPF library may return any result; we just verify no panic/crash.
func TestCheck_NullSender(t *testing.T) {
	ip := net.ParseIP("127.0.0.1")
	// Null sender is valid; result depends on DNS — we just ensure no panic.
	_, _ = Check(context.Background(), ip, "", "localhost")
}
