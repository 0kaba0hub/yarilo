package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/fts/language"
)

// ftsQueryStub answers Email/query from hits(text) and Email/get from
// subjects, so the check's judgement can be exercised without a cluster --
// the class of defect #1043 was about, where a live-only check hid a reading
// error behind a deploy.
func ftsQueryStub(t *testing.T, hits func(text string) []string, subjects map[string]string) *int32 {
	t.Helper()
	var calls int32
	stubJMAP(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		_ = json.Unmarshal(raw, &req)
		var method string
		_ = json.Unmarshal(req.MethodCalls[0][0], &method)

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "Email/query":
			var args struct {
				Filter struct {
					Text string `json:"text"`
				} `json:"filter"`
			}
			_ = json.Unmarshal(req.MethodCalls[0][1], &args)
			ids, _ := json.Marshal(hits(args.Filter.Text))
			fmt.Fprintf(w, `{"methodResponses":[["Email/query",{"ids":%s,"queryState":"1"},"c0"]]}`, ids) //nolint:errcheck
		case "Email/get":
			var args struct {
				IDs []string `json:"ids"`
			}
			_ = json.Unmarshal(req.MethodCalls[0][1], &args)
			list := make([]string, 0, len(args.IDs))
			for _, id := range args.IDs {
				list = append(list, `{"id":"`+id+`","subject":"`+subjects[id]+`"}`)
			}
			fmt.Fprintf(w, `{"methodResponses":[["Email/get",{"list":[%s]},"c0"]]}`, strings.Join(list, ",")) //nolint:errcheck
		default:
			fmt.Fprint(w, `{"methodResponses":[["error",{"type":"unknownMethod"},"c0"]]}`) //nolint:errcheck
		}
	})
	return &calls
}

// failingStub answers every call with one method error type.
func failingStub(t *testing.T, errType string) *int32 {
	t.Helper()
	var calls int32
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"methodResponses":[["error",{"type":%q,"description":"stub"},"c0"]]}`, errType) //nolint:errcheck
	})
	return &calls
}

// The two types #1204 separated must lead to different behaviour here, or the
// distinction has no consumer: a broken lookup is final and must be reported
// as itself, not as "indexing never completed" three timeouts later.
func TestJMAPFTSQueryStopsOnAFinalFailure(t *testing.T) {
	// Every type but serverUnavailable is final here, and each must arrive as
	// itself: waiting one out reports the wrong cause three timeouts later.
	for _, errType := range []string{"serverFail", "unsupportedFilter", "invalidArguments"} {
		t.Run(errType+" is reported at once", func(t *testing.T) {
			calls := failingStub(t, errType)
			setFlag(t, flagTimeout, 300*time.Millisecond)

			err := assertFTSQueryFindsOnly("u1@example.com", ftsProbeMarker, ftsProbeAbsent, ftsProbeSubject)
			if err == nil {
				t.Fatal("a failing backend passed")
			}
			if !strings.Contains(err.Error(), errType) {
				t.Errorf("error %q does not name the failure it got", err)
			}
			if strings.Contains(err.Error(), "indexing never completed") {
				t.Errorf("error %q blames indexing for %s", err, errType)
			}
			if n := atomic.LoadInt32(calls); n != 1 {
				t.Errorf("made %d calls, want 1: a final failure was retried", n)
			}
		})
	}

	// The transient one keeps its retry, or the fix above would have bought
	// speed by giving up on a lag that passes on its own.
	t.Run("serverUnavailable is retried", func(t *testing.T) {
		calls := failingStub(t, "serverUnavailable")
		setFlag(t, flagTimeout, 700*time.Millisecond)

		if err := assertFTSQueryFindsOnly("u1@example.com", ftsProbeMarker, ftsProbeAbsent, ftsProbeSubject); err == nil {
			t.Fatal("a permanently unavailable backend passed")
		}
		if n := atomic.LoadInt32(calls); n < 2 {
			t.Errorf("made %d calls, want more than one: a transient failure was not retried", n)
		}
	})
}

const (
	ftsProbeMarker  = "jfts000000001hit"
	ftsProbeAbsent  = "jfts000000001gap"
	ftsProbeSubject = "jmap fts smoke " + ftsProbeMarker
)

// What the check is worth is what it refuses. Each row is a backend that would
// pass a naive "at least one hit" assertion while being wrong.
func TestJMAPFTSQueryRefusesTheWrongAnswers(t *testing.T) {
	cases := []struct {
		name     string
		hits     func(string) []string
		subjects map[string]string
		wantErr  string
	}{
		{
			name:     "the filter is applied",
			hits:     func(text string) []string { return map[string][]string{ftsProbeMarker: {"m1"}}[text] },
			subjects: map[string]string{"m1": ftsProbeSubject},
		},
		{
			// The whole mailbox contains the marker too.
			name:     "a backend that ignores the condition",
			hits:     func(string) []string { return []string{"m1", "m2"} },
			subjects: map[string]string{"m1": ftsProbeSubject, "m2": "unrelated"},
			wantErr:  "want exactly the delivered one",
		},
		{
			// One hit, but not the message that was delivered.
			name:     "a hit on a different message",
			hits:     func(string) []string { return []string{"m2"} },
			subjects: map[string]string{"m2": "unrelated"},
			wantErr:  "matched a different message",
		},
		{
			// The condition works for the delivered marker and not for the
			// absent one: still a filter that is not being applied.
			name: "a match for a marker that was never delivered",
			hits: func(text string) []string {
				if text == ftsProbeAbsent {
					return []string{"m3"}
				}
				return []string{"m1"}
			},
			subjects: map[string]string{"m1": ftsProbeSubject, "m3": "ghost"},
			wantErr:  "the condition is not being applied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = ftsQueryStub(t, tc.hits, tc.subjects)
			setFlag(t, flagTimeout, 200*time.Millisecond)

			err := assertFTSQueryFindsOnly("u1@example.com", ftsProbeMarker, ftsProbeAbsent, ftsProbeSubject)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("a correct backend was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a backend that %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not say %q", err, tc.wantErr)
			}
		})
	}
}

// The markers must be distinguishable after tokenisation, or the negative
// query proves nothing: the generic tokenizer caps a token at 30 bytes, so two
// markers sharing a longer prefix collapse to one term. That is what made the
// gate report a hit for a marker nobody ever delivered (#1213).
func TestProbeMarkersSurviveTokenisation(t *testing.T) {
	// The engine's own constant, not a copy: it is configurable, and a copy
	// would keep this test green while a lower cap merged the markers back
	// into one term -- the silence this whole check exists against.
	tokenMaxLen := language.DefaultTokenMaxLen

	marker, absent, _ := ftsProbeMarkers()
	for _, m := range []string{marker, absent} {
		if len(m) > tokenMaxLen {
			t.Errorf("marker %q is %d bytes: it is indexed and queried truncated", m, len(m))
		}
	}
	// Differing only past the cap is the same failure in a different dress.
	head := min(len(marker), len(absent))
	if head > tokenMaxLen {
		head = tokenMaxLen
	}
	if marker[:head] == absent[:head] {
		t.Errorf("markers %q and %q are identical for the first %d bytes: one term, two queries",
			marker, absent, head)
	}
}
