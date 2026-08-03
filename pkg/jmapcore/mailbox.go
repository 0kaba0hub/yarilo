package jmapcore

// Mailbox is the Mailbox object of RFC 8621 §2. Field order follows the RFC so
// a hand-read response matches the spec side by side.
type Mailbox struct {
	ID string `json:"id"`
	// Name is the leaf name, not the path: hierarchy is expressed by ParentID,
	// so a client never has to know the server's delimiter.
	Name     string  `json:"name"`
	ParentID *string `json:"parentId"`
	// Role is the IANA-registered purpose ("inbox", "sent", …), null when the
	// mailbox has none.
	Role      *string `json:"role"`
	SortOrder uint32  `json:"sortOrder"`

	TotalEmails  uint32 `json:"totalEmails"`
	UnreadEmails uint32 `json:"unreadEmails"`
	TotalThreads uint32 `json:"totalThreads"`
	// UnreadThreads counts threads with at least one unread message.
	UnreadThreads uint32 `json:"unreadThreads"`

	MyRights     MailboxRights `json:"myRights"`
	IsSubscribed bool          `json:"isSubscribed"`
}

// MailboxRights is the per-mailbox permission set of RFC 8621 §2. Every member
// is required: a client that cannot see a right assumes it is absent.
type MailboxRights struct {
	MayReadItems   bool `json:"mayReadItems"`
	MayAddItems    bool `json:"mayAddItems"`
	MayRemoveItems bool `json:"mayRemoveItems"`
	MaySetSeen     bool `json:"maySetSeen"`
	MaySetKeywords bool `json:"maySetKeywords"`
	MayCreateChild bool `json:"mayCreateChild"`
	MayRename      bool `json:"mayRename"`
	MayDelete      bool `json:"mayDelete"`
	MaySubmit      bool `json:"maySubmit"`
}

// Mailbox roles registered for JMAP (RFC 8621 §2, IANA "JMAP Mailbox Roles").
const (
	RoleAll       = "all"
	RoleArchive   = "archive"
	RoleDrafts    = "drafts"
	RoleFlagged   = "flagged"
	RoleImportant = "important"
	RoleInbox     = "inbox"
	RoleJunk      = "junk"
	RoleSent      = "sent"
	RoleTrash     = "trash"
)

// GetRequest is the shape every Foo/get takes (RFC 8620 §5.1).
type GetRequest struct {
	AccountID string `json:"accountId"`
	// IDs nil means "every object of this type" (§5.1). The distinction from an
	// empty array, which means "none", is why this is a pointer.
	IDs        *[]string `json:"ids"`
	Properties *[]string `json:"properties"`
}

// GetResponse is the shape every Foo/get returns (RFC 8620 §5.1).
type GetResponse[T any] struct {
	AccountID string `json:"accountId"`
	// State is the object type's state string. A client passes it back to
	// Foo/changes; it is not comparable across types.
	State    string   `json:"state"`
	List     []T      `json:"list"`
	NotFound []string `json:"notFound"`
}

// QueryRequest is the shape every Foo/query takes (RFC 8620 §5.5).
type QueryRequest struct {
	AccountID      string `json:"accountId"`
	Position       int    `json:"position"`
	Limit          *uint  `json:"limit"`
	CalculateTotal bool   `json:"calculateTotal"`
}

// MailboxQueryRequest is Mailbox/query (RFC 8621 §2.3).
type MailboxQueryRequest struct {
	QueryRequest
	Filter *MailboxFilter `json:"filter"`
	Sort   []Comparator   `json:"sort"`
}

// MailboxFilter is a filter condition. RFC 8620 §5.5 also allows an operator
// node (AND/OR/NOT) here; a request carrying one is refused with
// unsupportedFilter rather than silently matching everything.
type MailboxFilter struct {
	// Operator is set only when the client sent an operator node, which is how
	// that case is detected.
	Operator string `json:"operator"`

	ParentID     *string `json:"parentId"`
	Name         *string `json:"name"`
	Role         *string `json:"role"`
	HasAnyRole   *bool   `json:"hasAnyRole"`
	IsSubscribed *bool   `json:"isSubscribed"`
}

// Comparator is one sort key (RFC 8620 §5.5).
type Comparator struct {
	Property    string `json:"property"`
	IsAscending *bool  `json:"isAscending"`
}

// Ascending reports the direction, defaulting to true as the RFC requires.
func (c Comparator) Ascending() bool {
	return c.IsAscending == nil || *c.IsAscending
}

// Query error types raised by a Foo/query (RFC 8620 §5.5).
const (
	ErrUnsupportedFilter = "unsupportedFilter"
	ErrUnsupportedSort   = "unsupportedSort"
)

// QueryResponse is the shape every Foo/query returns (RFC 8620 §5.5).
type QueryResponse struct {
	AccountID string `json:"accountId"`
	// QueryState identifies the result set. Whether it can be used for an
	// incremental update is stated by CanCalculateChanges, not assumed.
	QueryState          string   `json:"queryState"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
	Position            int      `json:"position"`
	IDs                 []string `json:"ids"`
	Total               *uint    `json:"total,omitempty"`
	Limit               *uint    `json:"limit,omitempty"`
}
