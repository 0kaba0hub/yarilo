// Package mailauth defines message-level authentication interfaces.
// Implementations (DKIM, ARC, …) live in their own packages and are
// wired in by the backend — the transport layer (SMTP, JMAP, …) only
// depends on these interfaces.
package mailauth

import (
	"context"
	"io"
)

// Signer adds an authentication signature to an outbound message.
type Signer interface {
	Sign(ctx context.Context, senderDomain string, msg io.Reader) (io.Reader, error)
}

// Verifier checks authentication signatures on an inbound message.
type Verifier interface {
	Verify(ctx context.Context, msg io.Reader) ([]Result, error)
}

// Result is a single authentication check outcome.
type Result struct {
	Protocol string // "dkim", "arc", …
	Domain   string
	Pass     bool
}
