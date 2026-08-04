package jmapcore

import "sort"

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

// headerDerivedProperties are answered from the message's header block alone.
//
// Kept apart from the structural ones because they are what a mailbox listing
// asks for — subject and from on every row — and because answering them does
// not require descending into the MIME tree. Conflating the two made the most
// common request in a mail client pay for the rarest.
var headerDerivedProperties = map[string]bool{
	"headers":    true,
	"messageId":  true,
	"inReplyTo":  true,
	"references": true,
	"sender":     true,
	"from":       true,
	"to":         true,
	"cc":         true,
	"bcc":        true,
	"replyTo":    true,
	"subject":    true,
	"sentAt":     true,
}

// structureDerivedProperties need the MIME tree walked and its text parts
// decoded.
var structureDerivedProperties = map[string]bool{
	"bodyStructure": true,
	"bodyValues":    true,
	"textBody":      true,
	"htmlBody":      true,
	"attachments":   true,
	"hasAttachment": true,
	"preview":       true,
}

// NeedsMessage reports whether answering this request requires reading the
// message at all. A client that named only index-backed properties — id,
// mailboxIds, keywords, size, receivedAt — is answered without opening it.
// A client that named no properties gets the full default set, which does
// require the message.
func (r EmailGetRequest) NeedsMessage() bool {
	return r.NeedsHeaders() || r.NeedsStructure()
}

// NeedsHeaders reports whether any requested property comes from the header
// block.
func (r EmailGetRequest) NeedsHeaders() bool {
	if r.Properties == nil {
		return true
	}
	for _, p := range *r.Properties {
		if headerDerivedProperties[p] {
			return true
		}
	}
	return false
}

// NeedsStructure reports whether answering the request requires walking the
// MIME tree and decoding its parts.
//
// A request naming only header properties does not: it is the shape a client
// uses to render a message list, and the walk it used to pay for is the whole
// body of every message in that list.
func (r EmailGetRequest) NeedsStructure() bool {
	if r.WantsBodyValues() || r.Properties == nil {
		return true
	}
	for _, p := range *r.Properties {
		if structureDerivedProperties[p] {
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

// EmailQueryRequest is Email/query (RFC 8621 §4.4).
type EmailQueryRequest struct {
	QueryRequest
	Filter *EmailFilter `json:"filter"`
	Sort   []Comparator `json:"sort"`
}

// EmailFilter is a filter condition (RFC 8621 §4.4.1). An operator node
// (AND/OR/NOT) sets Operator, which is how that case is detected and refused
// rather than silently matching everything.
type EmailFilter struct {
	Operator string `json:"operator"`

	InMailbox          *string  `json:"inMailbox"`
	InMailboxOtherThan []string `json:"inMailboxOtherThan"`
	Before             *string  `json:"before"`
	After              *string  `json:"after"`
	MinSize            *uint32  `json:"minSize"`
	MaxSize            *uint32  `json:"maxSize"`
	HasKeyword         *string  `json:"hasKeyword"`
	NotKeyword         *string  `json:"notKeyword"`
	HasAttachment      *bool    `json:"hasAttachment"`

	// Text conditions need the full-text index. They are named here so a
	// request carrying one can be refused explicitly instead of being ignored,
	// which would return a confidently wrong result set.
	Text    *string  `json:"text"`
	From    *string  `json:"from"`
	To      *string  `json:"to"`
	Cc      *string  `json:"cc"`
	Bcc     *string  `json:"bcc"`
	Subject *string  `json:"subject"`
	Body    *string  `json:"body"`
	Header  []string `json:"header"`
}

// TextConditions reports which full-text conditions the filter carries, so a
// caller can name them in its refusal rather than saying "unsupported".
func (f *EmailFilter) TextConditions() []string {
	if f == nil {
		return nil
	}
	var out []string
	for name, set := range map[string]bool{
		"text": f.Text != nil, "from": f.From != nil, "to": f.To != nil,
		"cc": f.Cc != nil, "bcc": f.Bcc != nil, "subject": f.Subject != nil,
		"body": f.Body != nil, "header": len(f.Header) > 0,
	} {
		if set {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// EffectiveLimit resolves the client's limit against the server's ceiling, the
// same way EffectiveBodyBytes does for body values: the smaller wins, and a
// client naming none gets the ceiling rather than everything. RFC 8620 §5.5
// lets a server impose its own limit provided the response reports it.
func EffectiveLimit(clientLimit *uint, ceiling uint) uint {
	if ceiling == 0 {
		if clientLimit == nil {
			return 0
		}
		return *clientLimit
	}
	if clientLimit == nil || *clientLimit > ceiling {
		return ceiling
	}
	return *clientLimit
}

// Thread is the Thread object of RFC 8621 §3.
type Thread struct {
	ID string `json:"id"`
	// EmailIDs is ordered by receivedAt (§3). With one message per thread the
	// order is trivially satisfied.
	EmailIDs []string `json:"emailIds"`
}
