package jmapcore

import "testing"

// The split between header-derived and structure-derived properties decides
// whether a request walks the MIME tree. Putting a property in the wrong set
// fails in one of two ways, and only one of them is loud:
//
//   - a structural property treated as header-derived answers with an empty
//     field and no error at all;
//   - a header property treated as structural is merely slow.
//
// So each property is asserted individually, against the rule that names it.
func TestPropertyClassification(t *testing.T) {
	for _, tc := range []struct {
		property  string
		structure bool
	}{
		// Answerable from the header block.
		{"subject", false},
		{"from", false},
		{"to", false},
		{"cc", false},
		{"bcc", false},
		{"sender", false},
		{"replyTo", false},
		{"sentAt", false},
		{"messageId", false},
		{"inReplyTo", false},
		{"references", false},
		{"headers", false},
		// Need the MIME tree walked and its parts decoded.
		{"bodyStructure", true},
		{"bodyValues", true},
		{"textBody", true},
		{"htmlBody", true},
		{"attachments", true},
		{"hasAttachment", true},
		{"preview", true},
	} {
		t.Run(tc.property, func(t *testing.T) {
			props := []string{tc.property}
			req := EmailGetRequest{GetRequest: GetRequest{Properties: &props}}
			if got := req.NeedsStructure(); got != tc.structure {
				t.Errorf("NeedsStructure = %v, want %v — %s would be answered %s",
					got, tc.structure, tc.property,
					map[bool]string{true: "with wasted work", false: "empty, with no error"}[got])
			}
			if !req.NeedsMessage() {
				t.Errorf("NeedsMessage = false; %s cannot come from the index", tc.property)
			}
		})
	}
}

// Index-only properties touch neither.
func TestIndexOnlyPropertiesNeedNothing(t *testing.T) {
	props := []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt"}
	req := EmailGetRequest{GetRequest: GetRequest{Properties: &props}}
	if req.NeedsMessage() {
		t.Error("a request for index-backed properties opens the message")
	}
	if req.NeedsStructure() {
		t.Error("a request for index-backed properties walks the MIME tree")
	}
}

// Naming no properties means every property, so both are required. A client
// that omits the field is asking for everything, and answering it from the
// header block alone would silently drop the body-derived half.
func TestOmittedPropertiesRequireEverything(t *testing.T) {
	var req EmailGetRequest
	if !req.NeedsHeaders() || !req.NeedsStructure() {
		t.Errorf("headers=%v structure=%v; a request naming no properties asks for all of them",
			req.NeedsHeaders(), req.NeedsStructure())
	}
}

// Body values are requested by their own flags rather than through properties,
// so a request naming only header properties must still take the walk when it
// asks for them.
func TestBodyValueFlagsForceTheWalk(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  EmailGetRequest
	}{
		{"text", EmailGetRequest{FetchTextBodyValues: true}},
		{"html", EmailGetRequest{FetchHTMLBodyValues: true}},
		{"all", EmailGetRequest{FetchAllBodyValues: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			props := []string{"id", "subject"}
			req := tc.req
			req.Properties = &props
			if !req.NeedsStructure() {
				t.Error("body values were requested and the MIME tree would not be walked")
			}
		})
	}
}
