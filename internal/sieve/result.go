package sieve

import "github.com/foxcpp/go-sieve/interp"

// Delivery is a deliver-to-folder action produced by a Sieve script.
type Delivery struct {
	Folder     string
	Flags      []string
	Create     bool   // mailbox extension: create folder if absent
	SpecialUse string // special-use extension: locate by attribute
	Implicit   bool   // true when this delivery comes from implicit keep (no explicit keep/fileinto)
}

// Redirect is a forward-to-address action produced by a Sieve script.
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

// PipeAction is a vnd.yarilo.pipe action produced by a Sieve script.
type PipeAction struct {
	ProgramName string
	Args        []string
	Copy        bool
	Try         bool
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
	Redirects []Redirect
	// Pipes lists vnd.yarilo.pipe actions to execute.
	Pipes []PipeAction
	// VacationReplies lists auto-replies the script wants sent.
	VacationReplies []interp.VacationResponse
	// Notifications lists enotify actions (RFC 5435).
	// Only mailto: method is dispatched; other methods are logged and dropped.
	Notifications []interp.ActionNotify
	// Reports lists vnd.yarilo.report actions (ARF abuse reports, RFC 5965).
	Reports []interp.ActionReport
	// Message is the substituted message bytes when a script rewrote the
	// message (vnd.yarilo.filter, or RFC 5703 replace / enclose). Nil means the
	// message is unchanged and the caller should deliver the original.
	Message []byte
}
