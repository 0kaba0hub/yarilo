package imap_test

import (
	"errors"
	"fmt"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
)

// Through SEARCH, not through the classifier. The classifier was right twice
// while the field was red -- once because the answer was built beside it
// (#1411), once because the marker never crossed the wire (#1409) -- and both
// times a test on the function alone was green.
func TestSearchDuringAnFTSOutageTellsTheClientToRetry(t *testing.T) {
	tests := []struct {
		name     string
		lookErr  error
		wantCode imaplib.ResponseCode
	}{
		{
			name:     "a dependency of the fts service is unreachable",
			lookErr:  fmt.Errorf("ftsproto: server: userdb unreachable: %w", ftsproto.ErrUnavailable),
			wantCode: imaplib.ResponseCodeUnavailable,
		},
		{
			// A broken index is this server's problem and retrying will not
			// mend it. Answering "try again" would send the client round a
			// loop it cannot win.
			name:     "the index itself is broken",
			lookErr:  errors.New("ftsproto: server: flatcurve: shard is corrupt"),
			wantCode: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeFTS{lookupErr: tt.lookErr, lastUID: 1}
			// Read fallback off: with it on, a failed lookup degrades to the
			// sequential scan and the client never sees an answer to classify.
			c := startFTSTestServerWith(t, fake, false, t.TempDir(), func(o *imapserver.FTSOptions) {
				o.ReadFallback = false
			})
			appendBody(t, c, "Subject: hello\r\n\r\nneedle in here\r\n")
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}

			_, err := c.Search(&imaplib.SearchCriteria{Body: []string{"needle"}}, nil).Wait()
			if err == nil {
				t.Fatal("SEARCH succeeded while FTS was failing")
			}
			var imapErr *imaplib.Error
			if !errors.As(err, &imapErr) {
				t.Fatalf("not an IMAP error: %v", err)
			}
			if imapErr.Code != tt.wantCode {
				t.Errorf("code = %q, want %q (text: %q)", imapErr.Code, tt.wantCode, imapErr.Text)
			}
			// The code and the text have to agree: a permanent code beside
			// "try again" tells a client two things, and it believes the code.
			saysRetry := containsFold(string(imapErr.Text), "try again")
			if want := tt.wantCode == imaplib.ResponseCodeUnavailable; saysRetry != want {
				t.Errorf("text %q says retry = %v, but the code is %q", imapErr.Text, saysRetry, imapErr.Code)
			}
		})
	}
}

func containsFold(hay, needle string) bool {
	lower := func(s string) string {
		b := []byte(s)
		for i := range b {
			if b[i] >= 'A' && b[i] <= 'Z' {
				b[i] += 'a' - 'A'
			}
		}
		return string(b)
	}
	h, n := lower(hay), lower(needle)
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
