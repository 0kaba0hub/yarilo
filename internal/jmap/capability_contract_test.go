package jmap

import (
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// The session resource is a contract: what it advertises is what a client will
// use. A capability listed with no methods behind it sends the client down a
// path that fails, and a method whose capability is unadvertised is a method no
// conforming client will ever call -- work that exists and cannot be reached.
//
// Checked in both directions, because each catches the other's blind spot.
func TestAdvertisedCapabilitiesAndImplementedMethodsAgree(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)

	lazy := &lazyStore{storage: s.opts.Storage, user: testUser}
	defer lazy.close()
	reg := s.registry(lazy, testUser)
	session := jmapcore.BuildSession(s.opts.Limits, testUser)

	advertised := map[string]bool{}
	for name := range session.Capabilities {
		advertised[name] = true
	}
	// An account capability that the session level does not carry would be
	// invisible to a client reading the session the way the RFC describes.
	for _, acct := range session.Accounts {
		for name := range acct.AccountCapabilities {
			if !advertised[name] {
				t.Errorf("account capability %q is not advertised at the session level", name)
			}
		}
	}

	used := map[string]bool{}
	for method, entry := range reg {
		if entry.Capability == "" {
			t.Errorf("method %q declares no capability, so a client cannot know when it may call it", method)
			continue
		}
		used[entry.Capability] = true
		if !advertised[entry.Capability] {
			t.Errorf("method %q needs capability %q, which the session does not advertise -- no conforming client will call it",
				method, entry.Capability)
		}
	}

	for name := range advertised {
		if !used[name] {
			t.Errorf("capability %q is advertised with no method behind it; a client that uses it gets unknownMethod", name)
		}
	}
}

// The mail capability is advertised, so the methods a client reasonably reaches
// for after Foo/get have to answer -- with a result or with a refusal it can
// act on, never with unknownMethod, which reads as "this server is broken".
//
// The list is the sync surface of RFC 8621 as this phase defines it, and it is
// spelled out rather than derived from the registry: a test built from the
// registry agrees with whatever the registry happens to contain, including
// after something is deleted.
func TestTheSyncSurfaceIsReachable(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	lazy := &lazyStore{storage: s.opts.Storage, user: testUser}
	defer lazy.close()
	reg := s.registry(lazy, testUser)

	want := []string{
		"Core/echo",
		"Mailbox/get", "Mailbox/query", "Mailbox/changes", "Mailbox/queryChanges",
		"Email/get", "Email/query", "Email/changes", "Email/set", "Email/queryChanges",
		"Thread/get", "SearchSnippet/get",
	}
	sort.Strings(want)
	var missing []string
	for _, name := range want {
		if _, ok := reg[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("advertised but not reachable: %s", strings.Join(missing, ", "))
	}
}

// Email/queryChanges answers a refusal the client can act on rather than
// nothing at all. The distinction matters: unknownMethod says the server is
// broken or the capability was mis-advertised, while cannotCalculateChanges
// says "run the query again", which is the one thing that works.
func TestQueryChangesRefusesInsteadOfBeingAbsent(t *testing.T) {
	// Both, because the argument is the same for both: a client that reached
	// Foo/query reaches for Foo/queryChanges next, whichever type it is.
	for _, method := range []string{"Email/queryChanges", "Mailbox/queryChanges"} {
		t.Run(method, func(t *testing.T) {
			s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)
			_, errType := changesCall(t, s, method,
				`{"accountId":"`+testUser+`","sinceState":"1-ZQ"}`)
			if errType != "cannotCalculateChanges" {
				t.Errorf("%s answered %q, want cannotCalculateChanges", method, errType)
			}
		})
	}
}
