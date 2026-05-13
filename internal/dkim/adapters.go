package dkim

import (
	"bytes"
	"context"
	"io"

	"github.com/0kaba0hub/yarilo/internal/mailauth"
)

// MessageSigner implements mailauth.Signer using DKIM.
type MessageSigner struct {
	KeyProv KeyProvider
	Cfg     SignConfig
}

func (s *MessageSigner) Sign(ctx context.Context, senderDomain string, msg io.Reader) (io.Reader, error) {
	key, err := s.KeyProv.GetPrivateKey(ctx, senderDomain)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := Sign(&buf, msg, senderDomain, key, s.Cfg); err != nil {
		return nil, err
	}
	return &buf, nil
}

// MessageVerifier implements mailauth.Verifier using DKIM.
type MessageVerifier struct{}

func (v *MessageVerifier) Verify(_ context.Context, msg io.Reader) ([]mailauth.Result, error) {
	results, err := Verify(msg)
	if err != nil {
		return nil, err
	}
	out := make([]mailauth.Result, len(results))
	for i, r := range results {
		out[i] = mailauth.Result{Protocol: "dkim", Domain: r.Domain, Pass: r.Pass}
	}
	return out, nil
}
