package jmapcore

import "testing"

// Parsing decides which header a client gets and in what shape, so each form of
// the name is pinned rather than sampled.
func TestParseHeaderProperty(t *testing.T) {
	for _, tc := range []struct {
		property string
		name     string
		form     HeaderForm
		all      bool
		ok       bool
	}{
		{"header:List-Unsubscribe", "List-Unsubscribe", FormRaw, false, true},
		{"header:List-Unsubscribe:asURLs", "List-Unsubscribe", FormURLs, false, true},
		{"header:Received:all", "Received", FormRaw, true, true},
		{"header:Received:asText:all", "Received", FormText, true, true},
		{"header:To:asAddresses", "To", FormAddresses, false, true},
		{"header:To:asGroupedAddresses", "To", FormGroupedAddresses, false, true},
		{"header:Message-Id:asMessageIds", "Message-Id", FormMessageIDs, false, true},
		{"header:Date:asDate", "Date", FormDate, false, true},
		{"header:X-Spam-Status:asRaw", "X-Spam-Status", FormRaw, false, true},

		// Malformed. Each is refused rather than read as the nearest valid
		// thing: a client asking for asURLs and quietly receiving a string gets
		// a type it did not ask for and cannot detect.
		{property: "header:"},
		{property: "header::asText"},
		{property: "header:To:asNonsense"},
		{property: "header:To:asText:asRaw"},
		{property: "subject"},
		{property: "headers"},
	} {
		t.Run(tc.property, func(t *testing.T) {
			got, ok := ParseHeaderProperty(tc.property)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if !tc.ok {
				return
			}
			if got.Name != tc.name || got.Form != tc.form || got.All != tc.all {
				t.Errorf("= %+v, want name=%s form=%s all=%v", got, tc.name, tc.form, tc.all)
			}
			if got.Property != tc.property {
				t.Errorf("Property = %q, want the name as written — it is the response key", got.Property)
			}
		})
	}
}

// A header property is header-derived, so a request naming only one still opens
// the message and still skips the MIME walk.
func TestHeaderPropertyNeedsHeadersNotStructure(t *testing.T) {
	props := []string{"id", "header:List-Unsubscribe:asURLs"}
	req := EmailGetRequest{GetRequest: GetRequest{Properties: &props}}

	if !req.NeedsHeaders() {
		t.Error("NeedsHeaders = false; the header block is exactly what this needs")
	}
	if req.NeedsStructure() {
		t.Error("NeedsStructure = true; a header field does not require the MIME tree")
	}
	if !req.NeedsMessage() {
		t.Error("NeedsMessage = false; it cannot come from the index")
	}
	if hp := req.HeaderProperties(); len(hp) != 1 || hp[0].Form != FormURLs {
		t.Errorf("HeaderProperties = %+v, want the one parsed property", hp)
	}
}
