package jmapcore

import (
	"reflect"
	"sort"
	"testing"
)

// specMailboxProperties is every Mailbox property of RFC 8621 §2, and
// specMailboxRights every member of MailboxRights.
//
// Written out rather than derived for the reason the Email list is: a list
// built from the struct agrees with the struct by construction and can say
// nothing about a property that was never modelled. That is the failure mode
// strict validation turns from quiet into loud — a request the specification
// requires us to serve, refused as a typo.
var (
	specMailboxProperties = []string{
		"id", "name", "parentId", "role", "sortOrder",
		"totalEmails", "unreadEmails", "totalThreads", "unreadThreads",
		"myRights", "isSubscribed",
	}
	specMailboxRights = []string{
		"mayReadItems", "mayAddItems", "mayRemoveItems", "maySetSeen",
		"maySetKeywords", "mayCreateChild", "mayRename", "mayDelete", "maySubmit",
	}
	// §3: a Thread has exactly these.
	specThreadProperties = []string{"id", "emailIds"}
)

func TestMailboxMatchesTheSpecification(t *testing.T) {
	assertPropertySet(t, "Mailbox", reflect.TypeOf(Mailbox{}), specMailboxProperties)
}

func TestMailboxRightsMatchesTheSpecification(t *testing.T) {
	assertPropertySet(t, "MailboxRights", reflect.TypeOf(MailboxRights{}), specMailboxRights)
}

func TestThreadMatchesTheSpecification(t *testing.T) {
	assertPropertySet(t, "Thread", reflect.TypeOf(Thread{}), specThreadProperties)
}

// assertPropertySet checks both directions at once, because each catches
// something the other cannot: a missing property is a request refused as a
// typo, and an extra one is an unannounced extension a client cannot know
// exists.
func assertPropertySet(t *testing.T, name string, typ reflect.Type, spec []string) {
	t.Helper()
	fields := jsonFields(typ)

	var missing []string
	inSpec := make(map[string]bool, len(spec))
	for _, p := range spec {
		inSpec[p] = true
		if _, ok := fields[p]; !ok {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s: specified properties with no field: %v — each is now refused as unknown", name, missing)
	}

	var extra []string
	for field := range fields {
		if !inSpec[field] {
			extra = append(extra, field)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("%s: fields outside the specification: %v", name, extra)
	}
}
