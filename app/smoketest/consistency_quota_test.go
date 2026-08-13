package main

import (
	"strings"
	"testing"
)

// The quota row against a stub: the two surfaces report the same account's
// usage, and a divergence is what neither of them can see alone, because each
// is self-consistent.
func TestQuotaRowJudgement(t *testing.T) {
	imapSide := func(used, limit string) *reading {
		return newReading(surfIMAP).field("storageUsedKiB", used).field("storageLimitKiB", limit)
	}
	adminSide := func(used, limit string) *reading {
		return newReading(surfAdminAPI).field("storageUsedKiB", used).field("storageLimitKiB", limit)
	}

	tests := []struct {
		name   string
		left   *reading
		right  *reading
		wantIn string
	}{
		{"the same numbers", imapSide("2048", "10240"), adminSide("2048", "10240"), ""},
		{"a different usage", imapSide("2048", "10240"), adminSide("4096", "10240"), "storageUsedKiB"},
		{"a different limit", imapSide("2048", "10240"), adminSide("2048", "20480"), "storageLimitKiB"},
		// Unlimited is 0 over IMAP and -1 over the admin API; the reader
		// converts, so by the time the judge sees them they are one number.
		// A judge that tolerated the pair would also tolerate a real 0 against
		// a real -1 elsewhere.
		{"unlimited, once converted", imapSide("2048", "0"), adminSide("2048", "0"), ""},
		{"unlimited against a limit", imapSide("2048", "0"), adminSide("2048", "10240"), "storageLimitKiB"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := judgeRow("quota", tc.left, tc.right, defaultAllowances())
			if tc.wantIn == "" {
				if err != nil {
					t.Fatalf("agreeing quota numbers were refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("diverging quota numbers were accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("verdict %q does not name %q", err, tc.wantIn)
			}
		})
	}
}

// The IMAP QUOTA response is scraped, so the scraping is pinned: a parser that
// silently returns zeros would make every quota row agree with an admin API
// that also reported zero for an account it could not read.
func TestParseQuotaStorage(t *testing.T) {
	tests := []struct {
		name              string
		lines             []string
		wantUsed, wantLim int64
		wantOK            bool
	}{
		{
			name:     "storage alone",
			lines:    []string{`* QUOTAROOT "INBOX" "User quota"`, `* QUOTA "User quota" (STORAGE 2048 10240)`},
			wantUsed: 2048, wantLim: 10240, wantOK: true,
		},
		{
			name:     "storage beside another resource",
			lines:    []string{`* QUOTA "User quota" (MESSAGE 12 100 STORAGE 2048 10240)`},
			wantUsed: 2048, wantLim: 10240, wantOK: true,
		},
		{
			name:  "no storage resource at all",
			lines: []string{`* QUOTA "User quota" (MESSAGE 12 100)`},
		},
		{
			name:  "a response carrying no quota",
			lines: []string{`* CAPABILITY IMAP4rev1 QUOTA`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			used, limit, ok := parseQuotaStorage(tc.lines)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (used != tc.wantUsed || limit != tc.wantLim) {
				t.Errorf("(used, limit) = (%d, %d), want (%d, %d)", used, limit, tc.wantUsed, tc.wantLim)
			}
		})
	}
}
