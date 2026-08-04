package jmapcore

import (
	"reflect"
	"sort"
	"testing"
)

// specProperties is every Email property of RFC 8621 §4.1, written out.
//
// Written out rather than derived, because this is the one check that can catch
// something *missing*: a list built from the struct agrees with the struct by
// construction and would say nothing about a property yarilo never modelled.
var specProperties = []string{
	// §4.1.1 Metadata.
	"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt",
	// §4.1.2 Header fields parsed.
	"messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc",
	"replyTo", "subject", "sentAt",
	// §4.1.2.1 The header list itself.
	"headers",
	// §4.1.4 Body parts.
	"bodyStructure", "bodyValues", "textBody", "htmlBody", "attachments",
	"hasAttachment", "preview",
}

// indexOnlyProperties are answerable without opening the message, which is why
// they appear in neither classification.
var indexOnlyProperties = map[string]bool{
	"id": true, "blobId": true, "threadId": true, "mailboxIds": true,
	"keywords": true, "size": true, "receivedAt": true,
}

// The classification decides what a request must do; validation decides what it
// may name. Both read the struct, so the two can only disagree if a name is
// classified without a field behind it — which is what "headers" was until
// #1034: requested, therefore the message was read, and answered with nothing.
//
// After #1035 that shape is worse than wasteful. A property classified with no
// field is refused outright, so a request the specification requires us to
// serve is answered with an error.
func TestEveryClassifiedPropertyHasAField(t *testing.T) {
	fields := jsonFields(reflect.TypeOf(Email{}))
	for _, set := range []map[string]bool{headerDerivedProperties, structureDerivedProperties} {
		for name := range set {
			if _, ok := fields[name]; !ok {
				t.Errorf("%q is classified but the Email object has no such field: a request naming it "+
					"is refused as unknown, though the specification requires it to be served", name)
			}
		}
	}
}

// The other direction: a field nobody classified is answered from the index,
// and if that is not true it comes back as a zero value with no error — the
// failure #1031 exists to prevent.
func TestEveryFieldIsClassifiedOrIndexOnly(t *testing.T) {
	for name := range jsonFields(reflect.TypeOf(Email{})) {
		switch {
		case indexOnlyProperties[name]:
		case headerDerivedProperties[name]:
		case structureDerivedProperties[name]:
		default:
			t.Errorf("%q is a field of Email and is in no classification, so it is treated as "+
				"answerable from the index; if it is not, it comes back empty and stated as fact", name)
		}
	}
}

// A property of §4.1 that yarilo does not model is refused as a typo, and the
// client is told its request is wrong when the request is right.
//
// This is the check the other two cannot make: they compare our own sets
// against our own struct, and agree with each other however much is missing.
func TestNoSpecifiedPropertyIsMissing(t *testing.T) {
	fields := jsonFields(reflect.TypeOf(Email{}))
	var missing []string
	for _, name := range specProperties {
		if _, ok := fields[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("RFC 8621 §4.1 properties with no field: %v — each is now refused as unknown", missing)
	}
}

// And the reverse, so the list above cannot rot into a subset of itself: a
// field with no place in §4.1 is an extension, and an unannounced one is a
// property a client cannot know exists.
func TestNoFieldIsOutsideTheSpecification(t *testing.T) {
	inSpec := make(map[string]bool, len(specProperties))
	for _, name := range specProperties {
		inSpec[name] = true
	}
	for name := range jsonFields(reflect.TypeOf(Email{})) {
		if !inSpec[name] {
			t.Errorf("%q is a field of Email and is not in RFC 8621 §4.1", name)
		}
	}
}
