package policy

import (
	"context"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// ProtocolAdapter wraps *Client so it satisfies the
// protocol.PolicyChecker interface. The protocol package
// intentionally does not import policy (to keep the build graph
// strictly bottom-up), so the type conversion between
// policy.Request and protocol.PolicyRequest happens here.
type ProtocolAdapter struct {
	C *Client
}

// CheckBefore satisfies protocol.PolicyChecker.
func (a ProtocolAdapter) CheckBefore(ctx context.Context, req protocol.PolicyRequest) (protocol.PolicyDecision, error) {
	d, err := a.C.CheckBefore(ctx, toRequest(req))
	return toDecision(d), err
}

// CheckAfter satisfies protocol.PolicyChecker.
func (a ProtocolAdapter) CheckAfter(ctx context.Context, req protocol.PolicyRequest, success, policyReject bool) (protocol.PolicyDecision, error) {
	d, err := a.C.CheckAfter(ctx, toRequest(req), success, policyReject)
	return toDecision(d), err
}

// ReportAfter satisfies protocol.PolicyChecker.
func (a ProtocolAdapter) ReportAfter(ctx context.Context, req protocol.PolicyRequest, success, policyReject bool) {
	a.C.ReportAfter(ctx, toRequest(req), success, policyReject)
}

func toRequest(r protocol.PolicyRequest) Request {
	return Request{
		Username:  r.Username,
		Password:  r.Password,
		RemoteIP:  r.RemoteIP,
		Service:   r.Service,
		DeviceID:  r.DeviceID,
		SessionID: r.SessionID,
		TLS:       r.TLS,
		FailType:  r.FailType,
	}
}

func toDecision(d Decision) protocol.PolicyDecision {
	return protocol.PolicyDecision{
		Continue:   d.Continue,
		Reject:     d.Reject,
		TarpitSecs: d.TarpitSecs,
		Message:    d.Message,
	}
}
