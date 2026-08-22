package jmap

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// storeFailure is the only place a Go error becomes a method error, so it is
// the only place that can tell a client "wait" apart from "this server is
// broken". The user resolver on this path reaches yarilo-auth per request
// (#1402), which is the second dependency it has to recognise.
func TestStoreFailureClassifiesEveryDependencyOutage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "auth unreachable",
			err:  fmt.Errorf("jmap: userdb u@example.com: %w", fmt.Errorf("authclient: dial: %w: %w", authclient.ErrUnavailable, errors.New("connection refused"))),
			want: jmapcore.ErrServerUnavailable,
		},
		{
			name: "lock service unreachable",
			err:  fmt.Errorf("subs: %w", locks.ErrUnavailable),
			want: jmapcore.ErrServerUnavailable,
		},
		{
			// An answer from auth, not a failure to reach it: the user is not
			// in the userdb. Retrying will return the same thing, so telling
			// the client to wait would be a lie it acts on.
			name: "user not found",
			err:  errors.New("jmap: userdb: user not found: u@example.com"),
			want: jmapcore.ErrServerFail,
		},
		{
			name: "an ordinary defect",
			err:  errors.New("jmap: storage is not wired"),
			want: jmapcore.ErrServerFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := storeFailure("mailbox state", "acc-1", tt.err)
			if got.Type != tt.want {
				t.Errorf("classified as %q, want %q (err: %v)", got.Type, tt.want, tt.err)
			}
		})
	}
}
