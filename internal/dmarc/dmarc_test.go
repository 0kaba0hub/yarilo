package dmarc

import (
	"testing"

	goSpf "blitiri.com.ar/go/spf"

	"github.com/0kaba0hub/yarilo/internal/mailauth"
)

var parseRecordCases = []struct {
	name string
	txt  string
	want Record
}{
	{
		"minimal",
		"v=DMARC1; p=none",
		Record{Policy: PolicyNone, SPPolicy: PolicyNone, ADKIM: AlignmentRelaxed, ASPF: AlignmentRelaxed},
	},
	{
		"reject with strict alignment",
		"v=DMARC1; p=reject; adkim=s; aspf=s",
		Record{Policy: PolicyReject, SPPolicy: PolicyReject, ADKIM: AlignmentStrict, ASPF: AlignmentStrict},
	},
	{
		"quarantine with subdomain policy",
		"v=DMARC1; p=quarantine; sp=none",
		Record{Policy: PolicyQuarantine, SPPolicy: PolicyNone, ADKIM: AlignmentRelaxed, ASPF: AlignmentRelaxed},
	},
}

func TestParseRecord(t *testing.T) {
	for _, tc := range parseRecordCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRecord(tc.txt)
			if err != nil {
				t.Fatalf("parseRecord error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

var orgDomainCases = []struct {
	input string
	want  string
}{
	{"example.com", "example.com"},
	{"mail.example.com", "example.com"},
	{"a.b.c.example.com", "example.com"},
	{"localhost", "localhost"},
}

func TestOrgDomain(t *testing.T) {
	for _, tc := range orgDomainCases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			got := orgDomain(tc.input)
			if got != tc.want {
				t.Errorf("orgDomain(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

var dkimAlignCases = []struct {
	name          string
	results       []mailauth.Result
	fromOrgDomain string
	mode          Alignment
	want          bool
}{
	{
		"pass relaxed same org",
		[]mailauth.Result{{Domain: "mail.example.com", Pass: true}},
		"example.com", AlignmentRelaxed, true,
	},
	{
		"pass strict — subdomain rejected",
		[]mailauth.Result{{Domain: "mail.example.com", Pass: true}},
		"example.com", AlignmentStrict, false,
	},
	{
		"pass strict — exact match",
		[]mailauth.Result{{Domain: "example.com", Pass: true}},
		"example.com", AlignmentStrict, true,
	},
	{
		"fail — no passing results",
		[]mailauth.Result{{Domain: "example.com", Pass: false}},
		"example.com", AlignmentRelaxed, false,
	},
	{
		"pass relaxed — different org",
		[]mailauth.Result{{Domain: "other.com", Pass: true}},
		"example.com", AlignmentRelaxed, false,
	},
}

func TestCheckDKIMAlignment(t *testing.T) {
	for _, tc := range dkimAlignCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := checkDKIMAlignment(tc.results, tc.fromOrgDomain, tc.mode)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

var spfAlignCases = []struct {
	name          string
	result        goSpf.Result
	spfDomain     string
	fromOrgDomain string
	mode          Alignment
	want          bool
}{
	{"pass relaxed", goSpf.Pass, "mail.example.com", "example.com", AlignmentRelaxed, true},
	{"pass strict subdomain", goSpf.Pass, "mail.example.com", "example.com", AlignmentStrict, false},
	{"pass strict exact", goSpf.Pass, "example.com", "example.com", AlignmentStrict, true},
	{"fail SPF", goSpf.Fail, "example.com", "example.com", AlignmentRelaxed, false},
	{"softfail SPF", goSpf.SoftFail, "example.com", "example.com", AlignmentRelaxed, false},
}

func TestCheckSPFAlignment(t *testing.T) {
	for _, tc := range spfAlignCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := checkSPFAlignment(tc.result, tc.spfDomain, tc.fromOrgDomain, tc.mode)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
