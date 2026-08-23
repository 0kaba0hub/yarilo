package jmap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Opening the store resolves the user against yarilo-auth, so this is where a
// dependency outage arrives first -- and it was answering serverFail while the
// classifier beside it already knew better (#1402). serverFail is final to a
// client: it stops retrying and shows its user an error, for an outage that
// ends in seconds.
func TestStoreOpenFailureClassifiesTheOutage(t *testing.T) {
	tests := []struct {
		name    string
		resolve func(string) (*mailbox.UserInfo, error)
		want    string
	}{
		{
			name: "auth unreachable while opening the store",
			resolve: func(string) (*mailbox.UserInfo, error) {
				return nil, fmt.Errorf("jmap: master dial: %w: %w", authclient.ErrUnavailable, errors.New("connection refused"))
			},
			want: jmapcore.ErrServerUnavailable,
		},
		{
			// auth answered: this user does not exist. Retrying returns the
			// same thing, so "try again" would be a lie the client acts on.
			name: "the user is not in the userdb",
			resolve: func(string) (*mailbox.UserInfo, error) {
				return nil, errors.New("jmap: userdb: user not found: u@example.com")
			},
			want: jmapcore.ErrServerFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			lazy := &lazyStore{
				storage: &Storage{
					Mailbox:     maildir.New(),
					Index:       fileindex.New(),
					Locker:      &testLocker{},
					ResolveUser: tt.resolve,
				},
				user: "u@example.com",
			}
			_, merr := s.withStore(lazy, "acc-1", func(*userHandle) (any, *jmapcore.MethodError) {
				t.Fatal("the method ran despite the store failing to open")
				return nil, nil
			})
			if merr == nil {
				t.Fatal("a store that could not be opened produced no error")
			}
			if merr.Type != tt.want {
				t.Errorf("classified as %q, want %q", merr.Type, tt.want)
			}
		})
	}
}

// The point of translation, not the call site. This bug was not a missing
// case in the classifier -- the classifier was already right. It was a second,
// hand-written answer beside it, which no amount of teaching the first one
// could reach (#1402).
//
// Widened from one file to the package (#1413): the same shape was sitting in
// five more places, and a guard that watches one file proves nothing about the
// next one written beside it. The exemptions are the other half of its value
// -- each is an error that CANNOT be a dependency outage, with the reason
// recorded, so the judgement is visible instead of implied by whoever wrote
// the line.
func TestNoHandWrittenDependencyAnswers(t *testing.T) {
	exempt := map[string]string{
		// The classifier itself: this is where the answer is built.
		"changes.go": "storeFailure lives here",
		// A folder with no GUID cannot be searched and retrying will not give
		// it one; and the busy/lagging arms are already classified by hand
		// because they are conditions of ours, not failures of a dependency.
		// Exactly three deliberate conditions remain there, none of them a
		// dependency failure: a folder with no GUID (permanent), the busy and
		// lagging arms (ours, already answering serverUnavailable), and a
		// caller's own cancellation. Everything else goes to storeFailure.
		"ftsquery.go": "conditions of this service only: no-GUID is permanent; busy/lagging and cancellation are ours and already classified",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if reason, ok := exempt[f]; ok {
			if reason == "" {
				t.Errorf("%s is exempt with no reason", f)
			}
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "ErrServerFail") || strings.Contains(line, "ErrServerUnavailable") {
				t.Errorf("%s:%d builds its own dependency answer: %s\n"+
					"classify through storeFailure -- one outage, one story -- or exempt the file with the reason it cannot be one",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
