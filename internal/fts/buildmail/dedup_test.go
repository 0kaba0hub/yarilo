package buildmail

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/fts/language"
	"github.com/0kaba0hub/yarilo/pkg/fts"
)

const alternativeTwinMsg = "Subject: dup\r\n" +
	"Content-Type: multipart/alternative; boundary=BOUND\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Quarterly results are excellent this year\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Quarterly results are excellent this year</p>\r\n" +
	"--BOUND--\r\n"

// TestDedupSkipsMultipartAlternativeTwin (#669): a text/plain and its
// multipart/alternative text/html twin carry the same content — with
// DedupBodyParts on, the second body part must not be tokenized again.
func TestDedupSkipsMultipartAlternativeTwin(t *testing.T) {
	upd := &fakeUpdate{}
	b := New(Options{DedupBodyParts: true}, mustChain(t))
	if err := b.Build(1, strings.NewReader(alternativeTwinMsg), upd); err != nil {
		t.Fatal(err)
	}
	var bodyKeys int
	for _, k := range upd.keys {
		if k.key.Type == fts.KeyBodyPart {
			bodyKeys++
		}
	}
	if bodyKeys != 1 {
		t.Fatalf("expected exactly 1 body part indexed (dup skipped), got %d", bodyKeys)
	}
	if !hasToken(upd.bodyTokens(), "quarter") {
		t.Fatalf("the surviving body part must still be indexed: %q", upd.bodyTokens())
	}
}

// TestDedupOffIndexesBothTwins is the control: without DedupBodyParts, both
// the text/plain and text/html twin are indexed separately (existing,
// unchanged behaviour).
func TestDedupOffIndexesBothTwins(t *testing.T) {
	upd := &fakeUpdate{}
	b := New(Options{}, mustChain(t))
	if err := b.Build(1, strings.NewReader(alternativeTwinMsg), upd); err != nil {
		t.Fatal(err)
	}
	var bodyKeys int
	for _, k := range upd.keys {
		if k.key.Type == fts.KeyBodyPart {
			bodyKeys++
		}
	}
	if bodyKeys != 2 {
		t.Fatalf("expected both twins indexed when dedup is off, got %d", bodyKeys)
	}
}

// TestDedupDistinctContentBothIndexed proves dedup does not over-collapse:
// two DIFFERENT body parts within the same message must both survive.
func TestDedupDistinctContentBothIndexed(t *testing.T) {
	msg := "Subject: distinct\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"alpha bravo charlie\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"delta echo foxtrot\r\n" +
		"--BOUND--\r\n"
	upd := &fakeUpdate{}
	b := New(Options{DedupBodyParts: true}, mustChain(t))
	if err := b.Build(1, strings.NewReader(msg), upd); err != nil {
		t.Fatal(err)
	}
	var bodyKeys int
	for _, k := range upd.keys {
		if k.key.Type == fts.KeyBodyPart {
			bodyKeys++
		}
	}
	if bodyKeys != 2 {
		t.Fatalf("distinct content must not be deduped away, got %d body parts", bodyKeys)
	}
	tokens := upd.bodyTokens()
	if !hasToken(tokens, "alpha") || !hasToken(tokens, "delta") {
		t.Fatalf("both distinct bodies must be indexed: %q", tokens)
	}
}

// fakeDecoder is a test Decoder that returns a fixed text for a configured
// content type, and ok=false for anything else.
type fakeDecoder struct {
	forContentType string
	text           []byte
}

func (d *fakeDecoder) Decode(_ context.Context, contentType, _ string, body io.Reader) ([]byte, bool, error) {
	_, _ = io.Copy(io.Discard, body) // decoders must be able to read the body regardless
	if contentType != d.forContentType {
		return nil, false, nil
	}
	return d.text, true, nil
}

const pdfAttachmentMsg = "Subject: attachment\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"see attached\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJcTl8uXrp/Og0MTGCg==\r\n" +
	"--BOUND--\r\n"

// TestDecoderIndexesAttachmentText (#669): a configured Decoder's extracted
// text for a non-text/HTML part must be indexed as a KeyBodyPart.
func TestDecoderIndexesAttachmentText(t *testing.T) {
	upd := &fakeUpdate{}
	b := New(Options{
		Decoder: &fakeDecoder{forContentType: "application/pdf", text: []byte("invoice total ninety nine dollars")},
	}, mustChain(t))
	if err := b.Build(1, strings.NewReader(pdfAttachmentMsg), upd); err != nil {
		t.Fatal(err)
	}
	body := upd.bodyTokens()
	if !hasToken(body, "invoic") && !hasToken(body, "invoice") {
		t.Fatalf("decoded attachment text not indexed: %q", body)
	}
}

// TestNoDecoderSkipsAttachment is the control: a nil Decoder (the default,
// fts_decoder_driver=none) must leave binary parts unindexed, unchanged from
// pre-#669 behaviour.
func TestNoDecoderSkipsAttachment(t *testing.T) {
	upd := &fakeUpdate{}
	b := New(Options{}, mustChain(t))
	if err := b.Build(1, strings.NewReader(pdfAttachmentMsg), upd); err != nil {
		t.Fatal(err)
	}
	for _, k := range upd.keys {
		if k.key.Type == fts.KeyBodyPart && k.key.ContentType == "application/pdf" {
			t.Fatal("binary part must stay unindexed with no decoder configured")
		}
	}
}

// TestDecoderUnsupportedTypeSkips: a Decoder that declines the content type
// (ok=false) must be treated exactly like no decoder — no error, no index.
func TestDecoderUnsupportedTypeSkips(t *testing.T) {
	upd := &fakeUpdate{}
	b := New(Options{
		Decoder: &fakeDecoder{forContentType: "application/msword", text: []byte("unused")},
	}, mustChain(t))
	if err := b.Build(1, strings.NewReader(pdfAttachmentMsg), upd); err != nil {
		t.Fatal(err)
	}
	for _, k := range upd.keys {
		if k.key.Type == fts.KeyBodyPart && k.key.ContentType == "application/pdf" {
			t.Fatal("decoder declining the content type must not index anything")
		}
	}
}

var errFakeMidRead = fmt.Errorf("fake mid-read failure")

// producePartialThenError simulates a body read that succeeds for the first
// chunk, then fails genuinely (not the size-cap sentinel) — the scenario a
// real broken/truncated part reader can hit mid-stream.
func producePartialThenError(sink func([]byte) error) error {
	if err := sink([]byte("partial content before failure")); err != nil {
		return err
	}
	return errFakeMidRead
}

// TestBuildBodyTextPartialReadErrorConsistentAcrossDedup (review finding):
// a genuine mid-part read error must be tolerated identically whether
// DedupBodyParts is on or off — both must index the partial text collected
// before the error, not silently diverge (dedup previously discarded the
// whole part on any non-cap error, while the non-dedup path always kept
// whatever had already streamed into the tokenizer).
func TestBuildBodyTextPartialReadErrorConsistentAcrossDedup(t *testing.T) {
	for _, dedup := range []bool{false, true} {
		t.Run(fmt.Sprintf("dedup=%v", dedup), func(t *testing.T) {
			upd := &fakeUpdate{}
			b := New(Options{DedupBodyParts: dedup}, mustChain(t))
			set := language.DefaultSettings()
			chain, err := language.NewChain(set)
			if err != nil {
				t.Fatal(err)
			}
			st := &buildState{uid: 1, remaining: -1}
			if dedup {
				st.seenHashes = make(map[uint64]struct{})
			}
			if err := b.buildBodyText(st, chain, "text/plain", producePartialThenError, upd); err != nil {
				t.Fatalf("buildBodyText: %v", err)
			}
			if !hasToken(upd.bodyTokens(), "partial") {
				t.Fatalf("dedup=%v: partial text before the read error must still be indexed, got %q", dedup, upd.bodyTokens())
			}
		})
	}
}
