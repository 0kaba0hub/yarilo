package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ftsQueryStub answers Email/query from hits(text) and Email/get from
// subjects, so the check's judgement can be exercised without a cluster --
// the class of defect #1043 was about, where a live-only check hid a reading
// error behind a deploy.
func ftsQueryStub(t *testing.T, hits func(text string) []string, subjects map[string]string) {
	t.Helper()
	stubJMAP(t, func(w http.ResponseWriter, r *http.Request) {
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
}

const (
	ftsProbeMarker  = "jmapftsmarker1"
	ftsProbeAbsent  = ftsProbeMarker + "notdelivered"
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
			ftsQueryStub(t, tc.hits, tc.subjects)
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
