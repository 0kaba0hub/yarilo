package sieve

import "github.com/foxcpp/go-sieve/interp"

// Delivery is a deliver-to-folder action produced by a Sieve script.
type Delivery struct {
	Folder     string
	Flags      []string
	Create     bool   // mailbox extension: create folder if absent
	SpecialUse string // special-use extension: locate by attribute
}

// Redirect is a forward-to-address action produced by a Sieve script.
// Requires SMTP relay — collected by Filter but not executed until Phase 2.
type Redirect struct {
	Address string
	Copy    bool // :copy — message also delivered to primary folder
}

// RejectErr signals that the Sieve script rejected the message.
type RejectErr struct {
	Enhanced bool // true = ereject (RFC 5429 enhanced reject)
	Reason   string
}

func (e *RejectErr) Error() string {
	if e.Enhanced {
		return "ereject: " + e.Reason
	}
	return "reject: " + e.Reason
}

// FilterResult is the outcome of executing a Sieve script against a message.
// Deliveries and Reject are mutually exclusive.
type FilterResult struct {
	// Deliveries lists folders the message should be stored in.
	// Nil (with Reject == nil) means the script requested discard.
	Deliveries []Delivery
	// Reject is non-nil when the script requested message rejection.
	Reject *RejectErr
	// Redirects lists addresses to forward the message to.
	// Requires SMTP relay (Phase 2) — collected but not acted upon yet.
	Redirects []Redirect
	// VacationReplies lists auto-replies the script wants sent.
	// Requires SMTP relay (Phase 2) — collected but not acted upon yet.
	VacationReplies []interp.VacationResponse
}
