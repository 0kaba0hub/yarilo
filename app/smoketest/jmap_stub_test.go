package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// stubJMAP answers every batch with a canned envelope and points the flags at
// itself.
//
// This exists because the check it guards ran against a live cluster and
// nothing else, so a defect in how it reads a response could only be found by
// deploying. It was: jmapCall already returns the first method's arguments, and
// the check unwrapped the envelope a second time, which made every assertion
// below it unreachable. The failure said "returned nothing" about a response
// that plainly was not empty, on all three accounts, and the local tests stayed
// green because they covered the helpers and never the HTTP path (#1043).
func stubJMAP(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	setFlag(t, flagJMAPHost, parsed.Hostname())
	setFlag(t, flagJMAPPort, parsed.Port())
	setFlag(t, flagInsecure, true)
	setFlag(t, flagJMAPUser, "u1@example.com")
	setFlag(t, flagJMAPPass, "pw")
}

// setFlag sets a flag for one test and restores it afterwards.
func setFlag[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

const cannedMailboxes = `{"methodResponses":[["Mailbox/get",` +
	`{"accountId":"u1@example.com","state":"1","list":[` +
	`{"id":"mb-inbox","name":"INBOX"},{"id":"mb-smoke","name":"YariloSmoke"}],` +
	`"notFound":[]},"c0"]]}`

// The one that was broken: the mailbox is in the answer and the check has to
// find it.
func TestJMAPMailboxIDReadsTheAnswer(t *testing.T) {
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(cannedMailboxes)) //nolint:errcheck
	})

	id, err := jmapMailboxID(smokeFolder)
	if err != nil {
		t.Fatalf("jmapMailboxID: %v", err)
	}
	if id != "mb-smoke" {
		t.Errorf("id = %q, want mb-smoke", id)
	}
}

// A folder that is genuinely absent must still be reported as absent, so the
// fix above cannot be "return the first thing you see".
func TestJMAPMailboxIDReportsAMissingFolder(t *testing.T) {
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"methodResponses":[["Mailbox/get",` + //nolint:errcheck
			`{"accountId":"u1@example.com","list":[{"id":"mb-inbox","name":"INBOX"}],"notFound":[]},"c0"]]}`))
	})

	if _, err := jmapMailboxID(smokeFolder); err == nil {
		t.Error("a missing folder was reported as found")
	}
}

func TestJMAPNewestEmailReadsTheAnswer(t *testing.T) {
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"methodResponses":[["Email/query",` + //nolint:errcheck
			`{"accountId":"u1@example.com","ids":["email-1"],"queryState":"1"},"c0"]]}`))
	})

	id, err := jmapNewestEmail("mb-smoke")
	if err != nil {
		t.Fatalf("jmapNewestEmail: %v", err)
	}
	if id != "email-1" {
		t.Errorf("id = %q, want email-1", id)
	}
}

// The validation half was broken independently, and would have been even
// without the double unwrap: jmapCall turns a method error into a Go error, so
// the check would have reported the refusal it was looking for as a failure of
// the call.
func TestVerifyPropertyValidationAcceptsARefusal(t *testing.T) {
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"methodResponses":[["error",` + //nolint:errcheck
			`{"type":"invalidArguments","description":"unknown properties: ` +
			`\"header:List-Unsubscribe:asURL\", \"subjekt\"","arguments":["properties"]},"c0"]]}`))
	})

	if err := verifyPropertyValidation("email-1"); err != nil {
		t.Errorf("a correct refusal was reported as a failure: %v", err)
	}
}

// And a server that accepts the typo must fail the check, or it passes on the
// defect it exists to find.
func TestVerifyPropertyValidationRejectsAcceptance(t *testing.T) {
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"methodResponses":[["Email/get",` + //nolint:errcheck
			`{"accountId":"u1@example.com","list":[{"id":"email-1"}],"notFound":[]},"c0"]]}`))
	})

	if err := verifyPropertyValidation("email-1"); err == nil {
		t.Error("a server that accepted an unknown property passed the check")
	}
}

// The forms themselves, against an answer that is right — so the reader of the
// canned envelope is exercised, not only the parts that reject.
func TestVerifyHeaderFormsReadsAGoodAnswer(t *testing.T) {
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(cannedGoodEmail)) //nolint:errcheck
	})

	if err := verifyHeaderForms("email-1"); err != nil {
		t.Errorf("a correct answer was reported as a failure: %v", err)
	}
}

// A wrong type for one form must fail: asURLs answering with the raw string is
// the failure the form exists to catch.
func TestVerifyHeaderFormsRejectsAWrongType(t *testing.T) {
	stubJMAP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(cannedWrongURLType)) //nolint:errcheck
	})

	if err := verifyHeaderForms("email-1"); err == nil {
		t.Error("asURLs answering with a raw string passed the check")
	}
}
