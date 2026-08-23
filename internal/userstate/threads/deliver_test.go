package threads

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func recorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	return NewRecorder(NewCache(time.Minute)), filepath.Join(t.TempDir(), FileName)
}

func rawMessage(msgID, inReplyTo, refs, subject string) []byte {
	h := ""
	if msgID != "" {
		h += fmt.Sprintf("Message-ID: <%s>\r\n", msgID)
	}
	if inReplyTo != "" {
		h += fmt.Sprintf("In-Reply-To: <%s>\r\n", inReplyTo)
	}
	if refs != "" {
		h += fmt.Sprintf("References: %s\r\n", refs)
	}
	h += fmt.Sprintf("Subject: %s\r\n\r\nbody\r\n", subject)
	return []byte(h)
}

// The minting rule, and the reason for it: a thread id comes from the message
// that started the conversation, so the migration step walking the same history
// arrives at the same ids. That rebuildability is what the whole design leans
// on -- it is the argument for having no fsync on this path.
func TestANewConversationIsNamedAfterItsFirstMessage(t *testing.T) {
	rec, path := recorder(t)
	got, err := rec.Record("u@example.com", path, "guid-1", rawMessage("first@x", "", "", "Hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "guid-1" {
		t.Errorf("thread = %q, want the message's own guid", got)
	}
}

func TestAReplyJoinsTheConversation(t *testing.T) {
	rec, path := recorder(t)
	first, err := rec.Record("u@example.com", path, "guid-1", rawMessage("first@x", "", "", "Hello"))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := rec.Record("u@example.com", path, "guid-2", rawMessage("second@x", "first@x", "", "Re: Hello"))
	if err != nil {
		t.Fatal(err)
	}
	if reply != first {
		t.Errorf("the reply is in %q, the original in %q", reply, first)
	}

	// And it is on disk, not only in the fold: a delivery that threaded in
	// memory and wrote nothing would look right until the process restarts.
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := st.ThreadOfGUID("guid-2"); !ok || id != first {
		t.Errorf("on disk the reply reports %q/%v, want %q", id, ok, first)
	}
}

// Clients emit references in more than one shape, and a header that reads as
// one malformed identifier costs a join. Safe in direction -- never a wrong
// join -- but a lost one on a shape that is common in the wild.
func TestReferencesAreReadInEveryShapeClientsSend(t *testing.T) {
	tests := []struct {
		name string
		refs string
	}{
		{"space separated", "<a@x> <b@x>"},
		{"comma and space", "<a@x>, <b@x>"},
		{"comma, no space", "<a@x>,<b@x>"},
		{"folded across lines", "<a@x>\r\n\t<b@x>"},
		{"no brackets at all", "a@x b@x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, path := recorder(t)
			first, _ := rec.Record("u@example.com", path, "guid-a", rawMessage("a@x", "", "", "One"))
			if _, err := rec.Record("u@example.com", path, "guid-b", rawMessage("b@x", "", "", "Two")); err != nil {
				t.Fatal(err)
			}
			late, err := rec.Record("u@example.com", path, "guid-c",
				rawMessage("c@x", "", tt.refs, "Re: One"))
			if err != nil {
				t.Fatal(err)
			}
			if late != first {
				t.Errorf("the reply landed in %q, not in the conversation %q -- the references were not read", late, first)
			}
			st, _ := Load(path)
			if got := st.Threads(); len(got) != 1 {
				t.Errorf("threads = %v, want one: the merge did not happen", got)
			}
		})
	}
}

// Folding is what the cache exists to avoid, so a second delivery to the same
// account must not fold again. The recorder has to tell the cache about its own
// write for that to hold -- and without that call the fold happens on every
// delivery, silently, which is the O(account)-per-message trap wearing the
// cache as a disguise.
func TestASecondRecordDoesNotFoldAgain(t *testing.T) {
	cache := NewCache(time.Minute)
	rec := NewRecorder(cache)
	path := filepath.Join(t.TempDir(), FileName)

	if _, err := rec.Record("u@example.com", path, "guid-1", rawMessage("a@x", "", "", "One")); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Record("u@example.com", path, "guid-2", rawMessage("b@x", "a@x", "", "Re: One")); err != nil {
		t.Fatal(err)
	}
	if got := cache.Folds(); got != 1 {
		t.Errorf("folds = %d for two deliveries, want 1", got)
	}
}

// A late message that answers two conversations merges them, and the merge is
// recorded so the old id still answers.
func TestALateMessageMergesTwoConversations(t *testing.T) {
	rec, path := recorder(t)
	a, _ := rec.Record("u@example.com", path, "guid-a", rawMessage("a@x", "", "", "One"))
	b, _ := rec.Record("u@example.com", path, "guid-b", rawMessage("b@x", "", "", "Two"))
	if a == b {
		t.Fatal("two unrelated messages were threaded together")
	}

	late, err := rec.Record("u@example.com", path, "guid-c",
		rawMessage("c@x", "", "<a@x> <b@x>", "Re: One"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, guid := range []string{"guid-a", "guid-b", "guid-c"} {
		if id, _ := st.ThreadOfGUID(guid); id != late {
			t.Errorf("%s is in %q, want the merged conversation %q", guid, id, late)
		}
	}
	if got := st.Threads(); len(got) != 1 {
		t.Errorf("threads = %v, want one after the merge", got)
	}
}

// Delivery must not fail because a message has unparseable or missing headers.
// Mail is authoritative and the sidecar is derived: refusing the delivery would
// be trading mail for metadata.
func TestAMessageWithoutHeadersStillDelivers(t *testing.T) {
	rec, path := recorder(t)
	tests := []struct {
		name string
		raw  []byte
	}{
		{"no headers at all", []byte("this is not a message")},
		{"no message-id", rawMessage("", "", "", "Subject only")},
		{"empty", nil},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guid := fmt.Sprintf("guid-%d", i)
			got, err := rec.Record("u@example.com", path, guid, tt.raw)
			if err != nil {
				t.Fatalf("delivery refused over headers: %v", err)
			}
			if got != guid {
				t.Errorf("thread = %q, want its own guid", got)
			}
		})
	}
}

// Two messages with no identity and the same subject DO join, and that is the
// subject fallback working. The row is here so the behaviour is deliberate
// rather than discovered: it is the half of threading that guesses.
func TestSubjectJoinsWhenIdentityIsSilent(t *testing.T) {
	rec, path := recorder(t)
	first, _ := rec.Record("u@example.com", path, "guid-1", rawMessage("", "", "", "Quarterly report"))
	second, err := rec.Record("u@example.com", path, "guid-2", rawMessage("", "", "", "Re: Quarterly report"))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("the subject fallback did not join: %q vs %q", second, first)
	}
}

// Subject-less messages must not collapse into one enormous conversation --
// the failure mode of every naive implementation, and the reason an empty
// subject key never joins.
func TestSubjectlessMessagesStayApart(t *testing.T) {
	rec, path := recorder(t)
	a, _ := rec.Record("u@example.com", path, "guid-1", []byte("Message-ID: <a@x>\r\n\r\nbody\r\n"))
	b, _ := rec.Record("u@example.com", path, "guid-2", []byte("Message-ID: <b@x>\r\n\r\nbody\r\n"))
	if a == b {
		t.Errorf("two subject-less messages were threaded together as %q", a)
	}
}
