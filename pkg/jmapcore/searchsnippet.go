package jmapcore

// SearchSnippet is one message's highlighted fragments (RFC 8621 §5.1). Either
// field may be null: a query whose only hit is the subject has nothing to show
// in the body, and inventing a fragment would be worse than showing none.
type SearchSnippet struct {
	EmailID string  `json:"emailId"`
	Subject *string `json:"subject"`
	Preview *string `json:"preview"`
}

// SearchSnippetGetRequest is the method's arguments. The filter is the one the
// snippets highlight against -- normally the same one Email/query was given.
type SearchSnippetGetRequest struct {
	AccountID string       `json:"accountId"`
	Filter    *EmailFilter `json:"filter"`
	EmailIDs  []string     `json:"emailIds"`
}

// SearchSnippetGetResponse mirrors the /get shape: what was produced and what
// could not be (§5.1).
type SearchSnippetGetResponse struct {
	AccountID string          `json:"accountId"`
	List      []SearchSnippet `json:"list"`
	NotFound  []string        `json:"notFound"`
}
