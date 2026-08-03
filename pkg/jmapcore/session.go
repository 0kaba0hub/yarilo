// Package jmapcore is the JMAP protocol layer: the wire types of RFC 8620
// (core) and RFC 8621 (mail), and the encoding rules around them.
//
// It imports nothing from yarilo. The package is meant to become a standalone
// library, so a server's config, storage and logging reach it only as plain
// arguments.
package jmapcore

import "strings"

// Capability URNs advertised in the session resource.
const (
	CapCore = "urn:ietf:params:jmap:core"
	CapMail = "urn:ietf:params:jmap:mail"
)

// SessionPath is the well-known endpoint of RFC 8620 §2.2. A client is given
// only this URL and discovers everything else from what it returns.
const SessionPath = "/.well-known/jmap"

// Limits are the server's advertised bounds. Clients batch against them, so a
// caller must publish the same numbers it enforces.
type Limits struct {
	// BaseURL is the public origin clients reach the server on. It prefixes
	// every URL in the session resource, so a proxied deployment sets it to the
	// externally visible name, not the pod address.
	BaseURL               string
	MaxSizeUpload         int64
	MaxSizeRequest        int64
	MaxConcurrentRequests int
	MaxCallsInRequest     int
	MaxObjectsInGet       int
	MaxObjectsInSet       int
}

// Session is the session resource of RFC 8620 §2. Field order follows the RFC
// so a hand-read response matches the spec side by side.
type Session struct {
	Capabilities    map[string]any     `json:"capabilities"`
	Accounts        map[string]Account `json:"accounts"`
	PrimaryAccounts map[string]string  `json:"primaryAccounts"`
	Username        string             `json:"username"`
	APIURL          string             `json:"apiUrl"`
	DownloadURL     string             `json:"downloadUrl"`
	UploadURL       string             `json:"uploadUrl"`
	EventSourceURL  string             `json:"eventSourceUrl"`
	State           string             `json:"state"`
}

// Account describes one account the authenticated user can reach (RFC 8620 §2).
type Account struct {
	Name                string         `json:"name"`
	IsPersonal          bool           `json:"isPersonal"`
	IsReadOnly          bool           `json:"isReadOnly"`
	AccountCapabilities map[string]any `json:"accountCapabilities"`
}

// CoreCapability is the urn:ietf:params:jmap:core object (RFC 8620 §2).
type CoreCapability struct {
	MaxSizeUpload         int64    `json:"maxSizeUpload"`
	MaxConcurrentUpload   int      `json:"maxConcurrentUpload"`
	MaxSizeRequest        int64    `json:"maxSizeRequest"`
	MaxConcurrentRequests int      `json:"maxConcurrentRequests"`
	MaxCallsInRequest     int      `json:"maxCallsInRequest"`
	MaxObjectsInGet       int      `json:"maxObjectsInGet"`
	MaxObjectsInSet       int      `json:"maxObjectsInSet"`
	CollationAlgorithms   []string `json:"collationAlgorithms"`
}

// MailCapability is the per-account urn:ietf:params:jmap:mail object
// (RFC 8621 §1.6). Nulls mean "no limit" and must stay null rather than 0.
type MailCapability struct {
	MaxMailboxesPerEmail       *int     `json:"maxMailboxesPerEmail"`
	MaxMailboxDepth            *int     `json:"maxMailboxDepth"`
	MaxSizeMailboxName         int      `json:"maxSizeMailboxName"`
	MaxSizeAttachmentsPerEmail int64    `json:"maxSizeAttachmentsPerEmail"`
	EmailQuerySortOptions      []string `json:"emailQuerySortOptions"`
	MayCreateTopLevelMailbox   bool     `json:"mayCreateTopLevelMailbox"`
}

// CollationAlgorithms are the comparators the server implements. i;ascii-numeric
// is the only one RFC 8620 requires.
var CollationAlgorithms = []string{"i;ascii-numeric", "i;ascii-casemap", "i;octet"}

// BuildSession renders the session resource for one authenticated user.
// accountID is the username: one account per user, and RFC 8620 only requires
// the id to be stable and opaque to the client.
func BuildSession(lim Limits, username string) *Session {
	base := strings.TrimRight(lim.BaseURL, "/")
	account := Account{
		Name:                username,
		IsPersonal:          true,
		AccountCapabilities: map[string]any{CapMail: MailCapabilityFor()},
	}
	return &Session{
		Capabilities: map[string]any{
			CapCore: CoreCapabilityFor(lim),
			// Mail is advertised at the session level with an empty object;
			// the per-account limits live in accountCapabilities.
			CapMail: map[string]any{},
		},
		Accounts:        map[string]Account{username: account},
		PrimaryAccounts: map[string]string{CapMail: username},
		Username:        username,
		APIURL:          base + "/jmap/api/",
		DownloadURL:     base + "/jmap/download/{accountId}/{blobId}/{name}?accept={type}",
		UploadURL:       base + "/jmap/upload/{accountId}/",
		EventSourceURL:  base + "/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
		State:           SessionState(lim),
	}
}

// CoreCapabilityFor renders the core capability from the advertised limits.
func CoreCapabilityFor(lim Limits) CoreCapability {
	return CoreCapability{
		MaxSizeUpload:         lim.MaxSizeUpload,
		MaxConcurrentUpload:   lim.MaxConcurrentRequests,
		MaxSizeRequest:        lim.MaxSizeRequest,
		MaxConcurrentRequests: lim.MaxConcurrentRequests,
		MaxCallsInRequest:     lim.MaxCallsInRequest,
		MaxObjectsInGet:       lim.MaxObjectsInGet,
		MaxObjectsInSet:       lim.MaxObjectsInSet,
		CollationAlgorithms:   CollationAlgorithms,
	}
}

// MailCapabilityFor renders the per-account mail capability.
func MailCapabilityFor() MailCapability {
	return MailCapability{
		// Unlimited: a message may sit in any number of mailboxes and the
		// hierarchy has no fixed depth.
		MaxMailboxesPerEmail:       nil,
		MaxMailboxDepth:            nil,
		MaxSizeMailboxName:         255,
		MaxSizeAttachmentsPerEmail: 0,
		EmailQuerySortOptions:      []string{"receivedAt", "size"},
		MayCreateTopLevelMailbox:   true,
	}
}
