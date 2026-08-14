package main

import (
	"strconv"
	"strings"
	"testing"
)

// The search row against a stub. The corpus is the point: one message carries
// the term in the subject only, one in the body only, one in neither. A backend
// that searches bodies only agrees with itself and disagrees with IMAP — which
// is the shape that shipped green before this area existed (#1209).
func TestSearchRowJudgement(t *testing.T) {
	const (
		subj = "xconsistency-search-subj-1"
		body = "xconsistency-search-body-1"
		none = "xconsistency-search-none-1"
	)
	seeded := newReading("seeded").set("search", []string{subj, body})

	tests := []struct {
		name   string
		hits   []string
		wantIn string // "" = must agree with the seeded set
	}{
		{"both halves found, in either order", []string{body, subj}, ""},
		{"body only — the header half was never searched", []string{body}, subj},
		{"subject only", []string{subj}, body},
		{"a message carrying the term nowhere", []string{subj, body, none}, none},
		{"nothing found at all", nil, subj},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newReading(surfJMAP).set("search", tc.hits)
			err := judgeRow("query against the seeded set", seeded, got, defaultAllowances())
			if tc.wantIn == "" {
				if err != nil {
					t.Fatalf("the seeded set was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a wrong result set was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("verdict %q does not name %q", err, tc.wantIn)
			}
		})
	}
}

// Two surfaces wrong the same way agree with each other. The row therefore
// judges each side against the seeded expectation first, and this is the test
// that the pair-only comparison would have missed.
func TestSearchRowRefusesTwoSurfacesWrongTheSameWay(t *testing.T) {
	const subj, body = "xconsistency-search-subj-1", "xconsistency-search-body-1"
	seeded := newReading("seeded").set("search", []string{subj, body})
	bodyOnly := func(s surface) *reading { return newReading(s).set("search", []string{body}) }

	if err := judgeRow("pair", bodyOnly(surfIMAP), bodyOnly(surfJMAP), defaultAllowances()); err != nil {
		t.Fatalf("the pair comparison alone was expected to accept this: %v", err)
	}
	if err := judgeRow("against seeded", seeded, bodyOnly(surfIMAP), defaultAllowances()); err == nil {
		t.Error("judging against the seeded set accepted a body-only search")
	}
}

// The marker is what identifies one message across surfaces that name messages
// differently, so it has to survive both spellings of the subject.
func TestMarkerSurvivesBothSubjectSpellings(t *testing.T) {
	const marker = "xconsistency-identity-1700000000"
	decoded := "Rechnung für März €42 " + marker
	encoded := "=?utf-8?Q?Rechnung_f=C3=BCr_M=C3=A4rz_=E2=82=AC42?= " + marker

	if got := markerIn(decoded); got != marker {
		t.Errorf("marker from a decoded subject = %q, want %q", got, marker)
	}
	if got := markerIn(encoded); got != marker {
		t.Errorf("marker from an encoded subject = %q, want %q", got, marker)
	}
	if got := markerIn("Rechnung für März"); got != "" {
		t.Errorf("a subject with no marker yielded %q", got)
	}
}

// The row failed while both surfaces answered correctly, because the IMAP side
// read its hits back by scraping ENVELOPE and the server returned the subject
// as a literal — which the scraper did not understand (#1279). Two guards
// against that class now: the search row maps hits to markers by UID and parses
// nothing, and the envelope reader understands literals where it is still used.
func TestEnvelopeSubjectReadsLiteralsAndQuotedStrings(t *testing.T) {
	const subject = "=?utf-8?Q?Rechnung?= xconsistency-identity-1700000000"

	tests := []struct {
		name string
		body string
	}{
		{
			name: "quoted string",
			body: `* 1 FETCH (ENVELOPE ("Fri, 14 Aug 2026 09:30:00 +0300" "` + subject + `" NIL NIL))`,
		},
		{
			// cmd() joins the response lines with one space, so the literal's
			// bytes follow the brace and that separator.
			name: "literal",
			body: `* 1 FETCH (ENVELOPE ("Fri, 14 Aug 2026 09:30:00 +0300" {` +
				itoa(len(subject)) + `} ` + subject + ` NIL NIL))`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := envelopeSubject(tc.body)
			if got != subject {
				t.Errorf("subject = %q, want %q", got, subject)
			}
			if m := markerIn(got); m != "xconsistency-identity-1700000000" {
				t.Errorf("marker from the subject = %q", m)
			}
		})
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
