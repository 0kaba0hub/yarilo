package sieve

import (
	"context"
	"net/textproto"
	"testing"
)

func TestNormalizeScore(t *testing.T) {
	hdr := textproto.MIMEHeader{
		"X-Spam-Score": {"5"},
		"X-Virus":      {"0"},
		"X-Spam-Junk":  {"not-a-number"},
		"X-Spam-Trail": {"7.5 / 10 (high)"},
	}
	cases := []struct {
		name       string
		header     string
		max, scale float64
		wantVal    string
		wantTested bool
	}{
		{"half of max on 0-10", "X-Spam-Score", 10, 10, "5", true},
		{"percent scale", "X-Spam-Score", 10, 100, "50", true},
		{"virus 0-5 clean", "X-Virus", 5, 5, "0", true},
		{"leading float with trailing text", "X-Spam-Trail", 10, 10, "8", true},
		{"unconfigured header", "", 10, 10, "0", false},
		{"absent header", "X-Missing", 10, 10, "0", false},
		{"unparsable value", "X-Spam-Junk", 10, 10, "0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, tested := normalizeScore(hdr, tc.header, tc.max, tc.scale)
			if val != tc.wantVal || tested != tc.wantTested {
				t.Errorf("normalizeScore = (%q, %v), want (%q, %v)", val, tested, tc.wantVal, tc.wantTested)
			}
		})
	}
}

func TestPolicySpamVirusScore(t *testing.T) {
	p := &policy{
		hdr:        textproto.MIMEHeader{"X-Spam": {"8"}, "X-Virus": {"5"}},
		spamHeader: "X-Spam", spamMax: 10,
		virusHeader: "X-Virus", virusMax: 5,
	}
	if v, ok := p.SpamScore(context.Background(), false); v != "8" || !ok {
		t.Errorf("SpamScore = (%q,%v), want (8,true)", v, ok)
	}
	if v, ok := p.SpamScore(context.Background(), true); v != "80" || !ok {
		t.Errorf("SpamScore percent = (%q,%v), want (80,true)", v, ok)
	}
	if v, ok := p.VirusScore(context.Background()); v != "5" || !ok {
		t.Errorf("VirusScore = (%q,%v), want (5,true)", v, ok)
	}
	// Unbacked policy reports not-scanned.
	var empty policy
	if v, ok := empty.SpamScore(context.Background(), false); v != "0" || ok {
		t.Errorf("unbacked SpamScore = (%q,%v), want (0,false)", v, ok)
	}
}
