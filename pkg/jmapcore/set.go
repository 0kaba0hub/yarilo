package jmapcore

import "encoding/json"

// SetRequest is the shape every Foo/set takes (RFC 8620 §5.3).
//
// create and update carry per-object payloads whose shape depends on the type,
// so they stay raw here and are decoded by the method that understands them.
// The id order within them is not meaningful; the RFC's ordering rules are
// about create/update/destroy as phases, which is where this package leaves it.
type SetRequest struct {
	AccountID string `json:"accountId"`
	// IfInState is the state the client believes the account is in. A pointer
	// because absent means "apply regardless", which is not the same as an
	// empty string a client could send deliberately.
	IfInState *string                    `json:"ifInState"`
	Create    map[string]json.RawMessage `json:"create"`
	Update    map[string]json.RawMessage `json:"update"`
	Destroy   []string                   `json:"destroy"`
}

// SetResponse is the shape every Foo/set returns (RFC 8620 §5.3).
//
// The four maps are always emitted, empty rather than absent: a client reading
// notUpdated to find out what failed must not have to distinguish "nothing
// failed" from "the server did not say".
type SetResponse struct {
	AccountID string `json:"accountId"`
	// OldState is what the state was before this call, NewState after it. A
	// client uses the pair to know whether anything else changed in between.
	OldState     string               `json:"oldState"`
	NewState     string               `json:"newState"`
	Created      map[string]any       `json:"created"`
	Updated      map[string]any       `json:"updated"`
	Destroyed    []string             `json:"destroyed"`
	NotCreated   map[string]*SetError `json:"notCreated"`
	NotUpdated   map[string]*SetError `json:"notUpdated"`
	NotDestroyed map[string]*SetError `json:"notDestroyed"`
}

// NewSetResponse builds a response with every collection non-nil, so the JSON
// carries empty objects rather than nulls.
func NewSetResponse(accountID, state string) *SetResponse {
	return &SetResponse{
		AccountID:    accountID,
		OldState:     state,
		NewState:     state,
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]*SetError{},
		NotUpdated:   map[string]*SetError{},
		NotDestroyed: map[string]*SetError{},
	}
}

// SetError is the per-object failure of a Foo/set (RFC 8620 §5.3). It is not a
// MethodError: one object failing leaves the rest of the call standing, which
// is the whole point of the type.
type SetError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
}

// SetError types from RFC 8620 §5.3, plus one of ours.
const (
	SetErrForbidden    = "forbidden"
	SetErrOverQuota    = "overQuota"
	SetErrTooLarge     = "tooLarge"
	SetErrNotFound     = "notFound"
	SetErrInvalidPatch = "invalidPatch"
	SetErrWillDestroy  = "willDestroy"
	SetErrInvalidProps = "invalidProperties"
	SetErrSingleton    = "singleton"
	// SetErrNotImplemented is ours. §5.3 lets a method define further types,
	// and the alternative was worse: reporting an unbuilt operation as
	// "forbidden" tells a client it lacks permission and invites it to retry
	// as someone else, while a method-level error would fail the whole call
	// including the objects that did succeed.
	SetErrNotImplemented = "notImplemented"
)

// ErrStateMismatch is the method-level error for an ifInState that does not
// match (RFC 8620 §5.3).
const ErrStateMismatch = "stateMismatch"
