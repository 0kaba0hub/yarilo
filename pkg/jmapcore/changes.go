package jmapcore

// ChangesRequest is the shape every Foo/changes takes (RFC 8620 §5.2).
type ChangesRequest struct {
	AccountID  string `json:"accountId"`
	SinceState string `json:"sinceState"`
	// MaxChanges is a ceiling, not a page size: a server that cannot answer
	// within it returns tooManyChanges rather than a truncated list, which
	// would tell the client it had seen everything.
	MaxChanges *uint `json:"maxChanges"`
}

// ChangesResponse is the answer. The three lists are always emitted, empty
// rather than absent -- a client reading destroyed must not have to tell
// "nothing was deleted" from "the server did not say".
type ChangesResponse struct {
	AccountID string `json:"accountId"`
	OldState  string `json:"oldState"`
	NewState  string `json:"newState"`
	// HasMoreChanges is always false here: this server answers a window whole
	// or refuses it, so there is never a remainder to fetch.
	HasMoreChanges bool     `json:"hasMoreChanges"`
	Created        []string `json:"created"`
	Updated        []string `json:"updated"`
	Destroyed      []string `json:"destroyed"`
}

// Errors a Foo/changes can raise (RFC 8620 §5.2).
const (
	// ErrCannotCalculateChanges is the honest refusal: the client refetches.
	// Every way of not knowing ends here rather than in an empty answer.
	ErrCannotCalculateChanges = "cannotCalculateChanges"
	ErrTooManyChanges         = "tooManyChanges"
)
