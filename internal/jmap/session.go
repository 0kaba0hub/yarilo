// Package jmap implements the JMAP endpoint (RFC 8620 core, RFC 8621 mail).
// This phase serves the session resource and authenticates; the data methods
// arrive later.
package jmap

import (
	"strings"

	"github.com/yarilomail/yarilo/pkg/config"
)

// Capability URNs advertised in the session resource.
const (
	CapCore = "urn:ietf:params:jmap:core"
	CapMail = "urn:ietf:params:jmap:mail"
)

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

// CoreCapability is the urn:ietf:params:jmap:core object (RFC 8620 §2). The
// limits are advertised, not merely enforced: clients batch against them.
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

// i4 collation is the only algorithm RFC 8620 requires a server to implement.
var collationAlgorithms = []string{"i;ascii-numeric", "i;ascii-casemap", "i;octet"}

// buildSession renders the session resource for one authenticated user.
// accountID is the username: yarilo has a single account per user, and RFC 8620
// only requires the id to be stable and opaque to the client.
func buildSession(cfg config.JMAPProtocolConfig, username string) *Session {
	base := strings.TrimRight(cfg.BaseURL, "/")
	account := Account{
		Name:       username,
		IsPersonal: true,
		AccountCapabilities: map[string]any{
			CapMail: mailCapability(),
		},
	}
	return &Session{
		Capabilities: map[string]any{
			CapCore: coreCapability(cfg),
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
		// A client refetches the session when this changes. It is derived from
		// the advertised limits, so a config change invalidates it and nothing
		// else does while the methods are still absent.
		State: sessionState(cfg),
	}
}

func coreCapability(cfg config.JMAPProtocolConfig) CoreCapability {
	return CoreCapability{
		MaxSizeUpload:         cfg.MaxSizeUpload,
		MaxConcurrentUpload:   cfg.MaxConcurrentRequests,
		MaxSizeRequest:        cfg.MaxSizeRequest,
		MaxConcurrentRequests: cfg.MaxConcurrentRequests,
		MaxCallsInRequest:     cfg.MaxCallsInRequest,
		MaxObjectsInGet:       cfg.MaxObjectsInGet,
		MaxObjectsInSet:       cfg.MaxObjectsInSet,
		CollationAlgorithms:   collationAlgorithms,
	}
}

func mailCapability() MailCapability {
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
