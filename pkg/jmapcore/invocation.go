package jmapcore

import (
	"encoding/json"
	"fmt"
)

// Invocation is one method call or response (RFC 8620 §3.2). On the wire it is
// a three-element array, not an object, which is why it carries its own
// marshalling.
type Invocation struct {
	Name string
	Args json.RawMessage
	// CallID ties a response to the call that produced it and is what a
	// back-reference names.
	CallID string
}

// MarshalJSON renders the wire triple.
func (i Invocation) MarshalJSON() ([]byte, error) {
	args := i.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return json.Marshal([3]any{i.Name, args, i.CallID})
}

// UnmarshalJSON reads the wire triple. Anything that is not exactly three
// elements is a malformed request, not a method that failed: the server cannot
// tell which call it was meant to answer.
func (i *Invocation) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("jmapcore: invocation is not an array: %w", err)
	}
	if len(raw) != 3 {
		return fmt.Errorf("jmapcore: invocation has %d elements, want 3", len(raw))
	}
	if err := json.Unmarshal(raw[0], &i.Name); err != nil {
		return fmt.Errorf("jmapcore: invocation name: %w", err)
	}
	// null decodes into a nil map without error, so the object is checked
	// explicitly: a method reading args.foo on null would panic.
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw[1], &args); err != nil {
		return fmt.Errorf("jmapcore: invocation arguments: %w", err)
	}
	if args == nil {
		return fmt.Errorf("jmapcore: invocation arguments are null, want an object")
	}
	i.Args = raw[1]
	if err := json.Unmarshal(raw[2], &i.CallID); err != nil {
		return fmt.Errorf("jmapcore: invocation call id: %w", err)
	}
	return nil
}

// MethodError is a method-level failure (RFC 8620 §3.6.2). It travels as an
// "error" invocation in methodResponses, keeping the batch's shape: one
// response per call, in order.
type MethodError struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	// Arguments names the request arguments the error is about, which
	// invalidArguments carries so a client can point at what it sent rather
	// than re-reading the whole call (RFC 8620 §3.6.2).
	Arguments []string `json:"arguments,omitempty"`
}

// Method error types this package raises itself. A server adds its own.
const (
	ErrUnknownMethod               = "unknownMethod"
	ErrInvalidArguments            = "invalidArguments"
	ErrInvalidResultReference      = "invalidResultReference"
	ErrServerFail                  = "serverFail"
	ErrAccountNotFound             = "accountNotFound"
	ErrAccountNotSupportedByMethod = "accountNotSupportedByMethod"
	ErrRequestTooLarge             = "requestTooLarge"
	// ErrServerUnavailable is the temporary one (RFC 8620 §3.6.2): the client
	// may retry the same call unchanged. Distinct from serverFail, which a
	// client is entitled to treat as final -- an index still catching up is
	// not the same news as a broken lookup.
	ErrServerUnavailable = "serverUnavailable"
)

func (e *MethodError) Error() string {
	if e.Description == "" {
		return "jmap: " + e.Type
	}
	return "jmap: " + e.Type + ": " + e.Description
}

// errorInvocation renders a method failure as the response to callID.
func errorInvocation(e *MethodError, callID string) Invocation {
	args, err := json.Marshal(e)
	if err != nil {
		// A struct of two strings cannot fail to marshal; keep the batch shape
		// rather than dropping the response for this call.
		args = json.RawMessage(`{"type":"serverFail"}`)
	}
	return Invocation{Name: "error", Args: args, CallID: callID}
}
