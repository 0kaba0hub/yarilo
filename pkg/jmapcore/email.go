package jmapcore

// EmailAddress is one address in a header field (RFC 8621 §4.1.2.3).
type EmailAddress struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// Email is the Email object of RFC 8621 §4.1. Only the properties this server
// derives are present; a client asking for others gets them omitted rather than
// invented.
type Email struct {
	ID         string          `json:"id"`
	BlobID     string          `json:"blobId"`
	ThreadID   string          `json:"threadId"`
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`
	Size       uint32          `json:"size"`
	ReceivedAt string          `json:"receivedAt"`

	// Header-derived fields (§4.1.2).
	MessageID  []string       `json:"messageId"`
	InReplyTo  []string       `json:"inReplyTo"`
	References []string       `json:"references"`
	Sender     []EmailAddress `json:"sender"`
	From       []EmailAddress `json:"from"`
	To         []EmailAddress `json:"to"`
	CC         []EmailAddress `json:"cc"`
	BCC        []EmailAddress `json:"bcc"`
	ReplyTo    []EmailAddress `json:"replyTo"`
	Subject    *string        `json:"subject"`
	SentAt     *string        `json:"sentAt"`

	// Body (§4.1.4).
	BodyStructure *EmailBodyPart            `json:"bodyStructure,omitempty"`
	BodyValues    map[string]EmailBodyValue `json:"bodyValues"`
	TextBody      []EmailBodyPart           `json:"textBody"`
	HTMLBody      []EmailBodyPart           `json:"htmlBody"`
	Attachments   []EmailBodyPart           `json:"attachments"`
	HasAttachment bool                      `json:"hasAttachment"`
	Preview       string                    `json:"preview"`
}

// EmailBodyPart describes one part of the message (RFC 8621 §4.1.4).
type EmailBodyPart struct {
	PartID      *string         `json:"partId"`
	BlobID      *string         `json:"blobId"`
	Size        uint32          `json:"size"`
	Name        *string         `json:"name"`
	Type        string          `json:"type"`
	Charset     *string         `json:"charset"`
	Disposition *string         `json:"disposition"`
	CID         *string         `json:"cid"`
	Language    []string        `json:"language"`
	Location    *string         `json:"location"`
	SubParts    []EmailBodyPart `json:"subParts,omitempty"`
}

// EmailBodyValue carries a decoded body part (RFC 8621 §4.1.4).
type EmailBodyValue struct {
	Value string `json:"value"`
	// IsEncodingProblem reports that the part could not be decoded cleanly.
	IsEncodingProblem bool `json:"isEncodingProblem"`
	// IsTruncated must be true whenever the server returned less than the whole
	// part. A server may truncate; it may not do so silently.
	IsTruncated bool `json:"isTruncated"`
}

// EmailGetRequest is Email/get (RFC 8621 §4.2).
type EmailGetRequest struct {
	GetRequest
	BodyProperties      *[]string `json:"bodyProperties"`
	FetchTextBodyValues bool      `json:"fetchTextBodyValues"`
	FetchHTMLBodyValues bool      `json:"fetchHTMLBodyValues"`
	FetchAllBodyValues  bool      `json:"fetchAllBodyValues"`
	// MaxBodyValueBytes is the client's own cap (§4.2.2). Zero means the client
	// stated none, which is not the same as asking for everything: the server's
	// ceiling still applies.
	MaxBodyValueBytes uint32 `json:"maxBodyValueBytes"`
}

// WantsBodyValues reports whether any body value was asked for at all.
func (r EmailGetRequest) WantsBodyValues() bool {
	return r.FetchTextBodyValues || r.FetchHTMLBodyValues || r.FetchAllBodyValues
}

// bodyDerivedProperties are the Email properties that cannot be answered from
// the index alone: each one needs the message parsed.
var bodyDerivedProperties = map[string]bool{
	"bodyStructure": true,
	"bodyValues":    true,
	"textBody":      true,
	"htmlBody":      true,
	"attachments":   true,
	"hasAttachment": true,
	"preview":       true,
	"headers":       true,
	"messageId":     true,
	"inReplyTo":     true,
	"references":    true,
	"sender":        true,
	"from":          true,
	"to":            true,
	"cc":            true,
	"bcc":           true,
	"replyTo":       true,
	"subject":       true,
	"sentAt":        true,
}

// NeedsMessage reports whether answering this request requires reading the
// message at all. A client that named only index-backed properties — id,
// mailboxIds, keywords, size, receivedAt — is answered without opening it.
// A client that named no properties gets the full default set, which does
// require the message.
func (r EmailGetRequest) NeedsMessage() bool {
	if r.WantsBodyValues() || r.Properties == nil {
		return true
	}
	for _, p := range *r.Properties {
		if bodyDerivedProperties[p] {
			return true
		}
	}
	return false
}

// EffectiveBodyBytes resolves the client's cap against the server's ceiling.
// The smaller wins, and a client that named no cap gets the ceiling rather than
// everything — the ceiling is the operator's bound on work, not a default a
// client can opt out of.
func EffectiveBodyBytes(clientMax uint32, ceiling uint32) uint32 {
	switch {
	case ceiling == 0:
		return clientMax
	case clientMax == 0 || clientMax > ceiling:
		return ceiling
	default:
		return clientMax
	}
}
