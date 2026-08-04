package jmap

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// callRaw returns the method response whether it succeeded or failed.
func callRaw(t *testing.T, s *Server, method, args string) (name string, out map[string]any) {
	t.Helper()
	w := postAPIRaw(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["`+method+`",`+args+`,"c0"]]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.MethodResponses) == 0 {
		t.Fatalf("no method responses: %s", w.Body)
	}
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("response name: %v", err)
	}
	if err := json.Unmarshal(resp.MethodResponses[0][1], &out); err != nil {
		t.Fatalf("response args: %v", err)
	}
	return name, out
}

func firstObject(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	list, _ := out["list"].([]any)
	if len(list) == 0 {
		t.Fatalf("list is empty: %v", out)
	}
	obj, _ := list[0].(map[string]any)
	return obj
}

// A response carries the requested properties and no others (RFC 8620 §5.1).
//
// Nothing is saved by this — the counters are computed either way — so it is
// correctness rather than speed. What it buys is that the answer stops
// asserting values the client did not ask about.
func TestMailboxGetProjects(t *testing.T) {
	s := storedServer(t)

	_, out := callRaw(t, s, "Mailbox/get",
		`{"accountId":"u1@example.com","ids":null,"properties":["id","name"]}`)
	obj := firstObject(t, out)

	if _, ok := obj["name"]; !ok {
		t.Errorf("name is missing: %v", obj)
	}
	for _, unwanted := range []string{
		"parentId", "role", "sortOrder", "totalEmails", "unreadEmails",
		"totalThreads", "unreadThreads", "myRights", "isSubscribed",
	} {
		if _, present := obj[unwanted]; present {
			t.Errorf("%q is in the response and was not requested", unwanted)
		}
	}
}

// Naming no properties asks for all of them.
func TestMailboxGetWithoutPropertiesReturnsEverything(t *testing.T) {
	s := storedServer(t)

	_, out := callRaw(t, s, "Mailbox/get", `{"accountId":"u1@example.com","ids":null}`)
	obj := firstObject(t, out)
	for _, want := range []string{"id", "name", "role", "myRights", "totalEmails", "isSubscribed"} {
		if _, present := obj[want]; !present {
			t.Errorf("%q is missing from a response that named no properties", want)
		}
	}
}

// Thread has two properties and neither is expensive, so projection saves
// nothing here at all. It is in for §5.1 and for the symmetry: a client that
// asks the same way of every type should be answered the same way.
func TestThreadGetProjects(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	_, out := callRaw(t, s, "Thread/get",
		`{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id"]}`)
	obj := firstObject(t, out)

	if obj["id"] != id {
		t.Errorf("id = %v, want %s", obj["id"], id)
	}
	if _, present := obj["emailIds"]; present {
		t.Errorf("emailIds is in the response and was not requested: %v", obj)
	}
}

// The half that carries the value for these two types: an unknown name is
// refused, so a typo is distinguishable from a property yarilo has not
// implemented. Silence answers both the same way.
func TestUnknownPropertiesAreRefused(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	for _, tc := range []struct {
		method, args, named string
	}{
		{"Mailbox/get", `{"accountId":"u1@example.com","ids":null,"properties":["id","nmae"]}`, "nmae"},
		{"Mailbox/get", `{"accountId":"u1@example.com","ids":null,"properties":["id","subject"]}`, "subject"},
		{"Thread/get", `{"accountId":"u1@example.com","ids":["` + id + `"],"properties":["id","emailIDs"]}`, "emailIDs"},
	} {
		t.Run(tc.method+" "+tc.named, func(t *testing.T) {
			name, out := callRaw(t, s, tc.method, tc.args)
			if name != "error" || out["type"] != "invalidArguments" {
				t.Fatalf("answered %s %v, want invalidArguments", name, out)
			}
			desc, _ := out["description"].(string)
			if !strings.Contains(desc, tc.named) {
				t.Errorf("description %q does not name %q", desc, tc.named)
			}
		})
	}
}

// And every valid name is accepted, or the test above would pass on a server
// that refuses everything.
func TestKnownPropertiesAreAcceptedByBothTypes(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	for _, tc := range []struct{ method, args string }{
		{"Mailbox/get", `{"accountId":"u1@example.com","ids":null,"properties":` +
			`["id","name","parentId","role","sortOrder","totalEmails","unreadEmails",` +
			`"totalThreads","unreadThreads","myRights","isSubscribed"]}`},
		{"Thread/get", `{"accountId":"u1@example.com","ids":["` + id + `"],"properties":["id","emailIds"]}`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			if name, out := callRaw(t, s, tc.method, tc.args); name == "error" {
				t.Fatalf("a request naming only valid properties was refused: %v", out)
			}
		})
	}
}
