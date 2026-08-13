package main

import (
	"strings"
	"testing"
)

// The readers row against a stub: one delivery, several surfaces, and each
// compared to the anchor on the facts it can actually report.
func TestReadersRowJudgement(t *testing.T) {
	anchor := newReading(surfIMAP).
		field("id", "M1").
		field("size", "4096").
		field("internalDate", "14-Aug-2026 09:30:00 +0300").
		field("subject", consistencySubjectRaw)

	tests := []struct {
		name   string
		other  *reading
		wantIn string // "" = must agree
	}{
		{
			name: "pop3 agrees on the facts it has",
			other: newReading(surfPOP3).
				field("size", "4096").
				field("subject", consistencySubjectRaw),
		},
		{
			// The one real cross-protocol fact POP3 shares: octets of the same
			// stored message.
			name: "pop3 reports a different octet count",
			other: newReading(surfPOP3).
				field("size", "4097").
				field("subject", consistencySubjectRaw),
			wantIn: "size",
		},
		{
			name: "jmap agrees, spelling the subject its own way",
			other: newReading(surfJMAP).
				field("id", "M1").
				field("size", "4096").
				field("internalDate", "2026-08-14T06:30:00Z").
				field("subject", consistencySubjectDecoded),
		},
		{
			name: "jmap reports another message entirely",
			other: newReading(surfJMAP).
				field("id", "M2").
				field("size", "4096").
				field("internalDate", "2026-08-14T06:30:00Z").
				field("subject", consistencySubjectDecoded),
			wantIn: "id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := judgeRow("one delivery", sharedFields(anchor, tc.other), tc.other, defaultAllowances())
			if tc.wantIn == "" {
				if err != nil {
					t.Fatalf("a reader that agrees was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a disagreeing reader was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("verdict %q does not name %q", err, tc.wantIn)
			}
		})
	}
}

// Narrowing to shared facts must not become a way to compare nothing: a surface
// that reports none of the anchor's facts is a reading error, not agreement.
func TestSharedFieldsKeepsOnlyWhatBothReport(t *testing.T) {
	anchor := newReading(surfIMAP).field("id", "M1").field("size", "4096")
	pop3 := newReading(surfPOP3).field("size", "4096")

	shared := sharedFields(anchor, pop3)
	if _, ok := shared.fields["id"]; ok {
		t.Error("a fact POP3 cannot report was demanded of it")
	}
	if shared.fields["size"] != "4096" {
		t.Error("the shared fact was dropped")
	}
	if got := len(sharedFields(anchor, newReading(surfPOP3)).fields); got != 0 {
		t.Errorf("%d facts survived against a surface that reported nothing", got)
	}
}

// The POP3 reader recognises the probe by its subject header, folded or not.
func TestHeaderValueReadsFoldedHeaders(t *testing.T) {
	headers := []string{
		"From: consistency@test.invalid",
		"Subject: =?utf-8?Q?Rechnung?=",
		" xconsistency-readers-1700000000",
		"Date: Fri, 14 Aug 2026 09:30:00 +0300",
	}
	got := headerValue(headers, "Subject")
	if !strings.Contains(got, "xconsistency-readers-1700000000") {
		t.Errorf("folded subject read as %q", got)
	}
	if headerValue(headers, "X-Absent") != "" {
		t.Error("an absent header reported a value")
	}
}
