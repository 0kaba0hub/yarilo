package backend

import (
	"context"
	"errors"
	"testing"

	authclient "github.com/0kaba0hub/yarilo/pkg/authclient"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestLazyUserdbLookup_LazyAndSurfacesDialError guards #821: building the LMTP
// userdb lookup must NOT dial yarilo-auth (an eager dial wedged lmtp readiness,
// and hung under internal_tls). The dial happens on the first lookup, and a
// failed dial surfaces as an error — not a hang.
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

	// A subsequent lookup re-dials (the previous failure reset the client).
	_, _ = lookup(context.Background(), "u2@d.test")
	if dials != 2 {
		t.Fatalf("after a failed dial the next lookup must re-dial, dialed %d", dials)
	}
}
