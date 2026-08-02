package backend

import (
	"context"
	"errors"
	"testing"

	authclient "github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Building the lookup must not dial yarilo-auth; the dial happens on the
// first lookup and a failed dial returns an error instead of hanging (#821).
func TestLazyUserdbLookup_LazyAndSurfacesDialError(t *testing.T) {
	dials := 0
	boom := errors.New("connection refused")
	dial := func() (*authclient.Client, error) {
		dials++
		return nil, boom
	}

	lookup := lazyUserdbLookup("yarilo-auth:9102", dial, &mailbox.Resolver{})
	if dials != 0 {
		t.Fatalf("building the lookup must not dial (regression), dialed %d", dials)
	}

	_, err := lookup(context.Background(), "u@d.test")
	if dials != 1 {
		t.Fatalf("first lookup must dial exactly once, dialed %d", dials)
	}
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("a failed dial must surface (not hang), got %v", err)
	}

	// failed dial resets the client, so the next lookup re-dials
	_, _ = lookup(context.Background(), "u2@d.test")
	if dials != 2 {
		t.Fatalf("after a failed dial the next lookup must re-dial, dialed %d", dials)
	}
}
