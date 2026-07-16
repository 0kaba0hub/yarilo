package ftsproto

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/fts"
)

func TestQueryResultRoundTrip(t *testing.T) {
	q := fts.Query{
		Terms: []fts.Term{{
			Field:   fts.FieldHeader,
			HdrName: "subject",
			Words:   []fts.Word{{Variants: []string{"Foo Bar", "foo"}}},
			Phrase:  "Foo Bar",
			Not:     true,
		}},
		AndTerms: true,
	}
	enc, err := EncodeQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(enc, "\t\n") {
		t.Fatal("encoded query must be TAB/LF-safe")
	}
	got, err := DecodeQuery(enc)
	if err != nil || !reflect.DeepEqual(got, q) {
		t.Fatalf("query round-trip = %+v (%v)", got, err)
	}

	r := fts.Result{Definite: []uint32{1, 3}, Maybe: []uint32{7},
		Scores: []fts.Score{{UID: 1, Value: 0.5}}}
	encR, err := EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	gotR, err := DecodeResult(encR)
	if err != nil || !reflect.DeepEqual(gotR, r) {
		t.Fatalf("result round-trip = %+v (%v)", gotR, err)
	}
}

type stubService struct {
	lastCmd string
	lookup  fts.Result
}

func (s *stubService) Index(user string, m fts.MailboxRef, maxUID uint32, maxRecent int) error {
	s.lastCmd = "index"
	return nil
}
func (s *stubService) Prepend(string, fts.MailboxRef, uint32) error {
	s.lastCmd = "prepend"
	return nil
}
func (s *stubService) Expunge(string, fts.MailboxRef, uint32) error {
	s.lastCmd = "expunge"
	return nil
}
func (s *stubService) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	s.lastCmd = "lookup"
	return s.lookup, nil
}
func (s *stubService) Status(string, fts.MailboxRef) (uint32, uint32, error) {
	s.lastCmd = "status"
	return 42, 7, nil
}
func (s *stubService) Rescan(string, fts.MailboxRef) error {
	s.lastCmd = "rescan"
	return nil
}
func (s *stubService) Optimize(string) error {
	s.lastCmd = "optimize"
	return nil
}

func TestDispatch(t *testing.T) {
	svc := &stubService{lookup: fts.Result{Definite: []uint32{5}}}
	q, _ := EncodeQuery(fts.Query{Terms: []fts.Term{{Field: fts.FieldBody,
		Words: []fts.Word{{Variants: []string{"x"}}}}}})

	tests := []struct {
		name    string
		line    string
		wantPfx string
		wantCmd string
	}{
		{"handshake", "VERSION\t1", "VERSION\t1\tOK", ""},
		{"handshake bad version", "VERSION\t9", "NO\t", ""},
		{"index", "INDEX\tu@x\tINBOX\tg1\t1\t10\t0", "OK", "index"},
		{"prepend", "PREPEND\tu@x\tINBOX\tg1\t1\t10", "OK", "prepend"},
		{"expunge", "EXPUNGE\tu@x\tINBOX\tg1\t1\t5", "OK", "expunge"},
		{"lookup", "LOOKUP\tu@x\tINBOX\tg1\t1\t" + q, "OK\t", "lookup"},
		{"status", "STATUS\tu@x\tINBOX\tg1\t1", "OK\t42\t7", "status"},
		{"rescan", "RESCAN\tu@x\tINBOX\tg1\t1", "OK", "rescan"},
		{"optimize", "OPTIMIZE\tu@x", "OK", "optimize"},
		{"malformed index", "INDEX\tu@x\tINBOX", "NO\t", ""},
		{"bad uidvalidity", "STATUS\tu@x\tINBOX\tg1\tzz", "NO\t", ""},
		{"unknown command", "BOGUS\tx", "NO\t", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc.lastCmd = ""
			got := dispatch(tc.line, svc)
			if !strings.HasPrefix(got, tc.wantPfx) {
				t.Fatalf("dispatch(%q) = %q, want prefix %q", tc.line, got, tc.wantPfx)
			}
			if tc.wantCmd != "" && svc.lastCmd != tc.wantCmd {
				t.Fatalf("dispatched to %q, want %q", svc.lastCmd, tc.wantCmd)
			}
		})
	}
}
