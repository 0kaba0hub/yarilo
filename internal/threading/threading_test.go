package threading

import (
	"reflect"
	"testing"
)

// The corpus the design asked for. Threading's reputation is made here: a
// subject join that is too eager puts strangers in one conversation, and one
// that is too shy threads nothing for anyone writing in a language whose
// clients do not say "Re:".
func TestNormalizeSubject(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{"plain", "Project status", "project status"},
		{"english reply", "Re: Project status", "project status"},
		{"english forward", "Fwd: Project status", "project status"},
		{"stacked", "Re: Fwd: Re: Project status", "project status"},
		{"counter", "Re[2]: Project status", "project status"},
		{"counter in parens", "Re(3): Project status", "project status"},
		{"case and spacing", "  RE:   Project    status ", "project status"},
		{"trailing fwd", "Project status (fwd)", "project status"},
		// Non-English clients, which is most of them somewhere.
		{"german", "AW: Projektstatus", "projektstatus"},
		{"german forward", "WG: Projektstatus", "projektstatus"},
		{"swedish", "SV: Projektstatus", "projektstatus"},
		{"polish", "Odp: Status projektu", "status projektu"},
		{"dutch forward", "Doorst: Projectstatus", "projectstatus"},
		// The ones that must NOT be stripped: a prefix is only a prefix when a
		// colon follows, or every subject starting with "reset" loses its head.
		{"word starting with re", "Reset the counters", "reset the counters"},
		{"word starting with fw", "Fwd rules discussion", "fwd rules discussion"},
		{"no colon", "RE the meeting", "re the meeting"},
		// An empty key never joins, and this is where that starts.
		{"empty", "", ""},
		{"only a prefix", "Re:", ""},
		{"only prefixes", "Re: Fwd: Re:", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeSubject(tt.subject); got != tt.want {
				t.Errorf("NormalizeSubject(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}

// knownState is the caller's storage, as far as this package can see it.
type knownState struct {
	byMessage map[string]string
	bySubject map[string]string
}

func (k knownState) ThreadOfMessage(id string) (string, bool) {
	t, ok := k.byMessage[id]
	return t, ok
}

func (k knownState) ThreadOfSubject(key string) (string, bool) {
	t, ok := k.bySubject[key]
	return t, ok
}

func TestResolvePlacesAMessage(t *testing.T) {
	known := knownState{
		byMessage: map[string]string{
			"root@a":   "T1",
			"reply@a":  "T1",
			"other@b":  "T2",
			"single@c": "T3",
		},
		// An entry under the empty key, which is what a caller that stored
		// subject-less messages by their (empty) key would have. Without the
		// guard in Resolve, every subject-less message in the account would
		// join this one thread.
		bySubject: map[string]string{"project status": "T1", "": "TEMPTY"},
	}

	tests := []struct {
		name       string
		msg        Message
		wantThread string
		wantMerged []string
	}{
		{
			name:       "answers a known message",
			msg:        Message{MessageID: "new@x", InReplyTo: []string{"reply@a"}, Subject: "Re: whatever"},
			wantThread: "T1",
		},
		{
			// The decision recorded on #1425: a late message that joins two
			// conversations merges them. A split thread is a permanent wrong
			// answer; a merge is a one-off change the protocol carries.
			name: "joins two threads and merges them",
			msg: Message{MessageID: "late@x", References: []string{"root@a", "other@b"},
				Subject: "Re: project status"},
			wantThread: "T1",
			wantMerged: []string{"T2"},
		},
		{
			// Identity beats subject, always. A reply whose subject was
			// rewritten still belongs where its References say.
			name: "identity wins over a subject that points elsewhere",
			msg: Message{MessageID: "renamed@x", InReplyTo: []string{"single@c"},
				Subject: "Re: project status"},
			wantThread: "T3",
		},
		{
			name:       "no identity, subject joins",
			msg:        Message{MessageID: "subjonly@x", Subject: "Re: Project status"},
			wantThread: "T1",
		},
		{
			name:       "nothing to join",
			msg:        Message{MessageID: "fresh@x", Subject: "Something new"},
			wantThread: "",
		},
		{
			// Every naive implementation threads a mailbox of subject-less
			// messages into one enormous conversation. An empty key never
			// joins -- and the state above contains a thread under "" so this
			// row can tell the difference.
			name:       "an empty subject joins nothing",
			msg:        Message{MessageID: "quiet@x", Subject: "   "},
			wantThread: "",
		},
		{
			// A message does not answer itself: a malformed client that echoes
			// its own id into References must not join everything that shares
			// the malformation.
			name:       "its own id is not a reference",
			msg:        Message{MessageID: "root@a", References: []string{"root@a"}, Subject: "New topic"},
			wantThread: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.msg, known)
			if got.ThreadID != tt.wantThread {
				t.Errorf("thread = %q, want %q", got.ThreadID, tt.wantThread)
			}
			if len(got.MergedFrom) != len(tt.wantMerged) ||
				(len(tt.wantMerged) > 0 && !reflect.DeepEqual(got.MergedFrom, tt.wantMerged)) {
				t.Errorf("merged = %v, want %v", got.MergedFrom, tt.wantMerged)
			}
		})
	}
}

// Two processes threading the same delivery must place it identically, or the
// same mailbox reads differently depending on who wrote it last. The order of
// the surviving thread is therefore part of the contract, not an accident of
// map iteration.
func TestMergeIsDeterministic(t *testing.T) {
	known := knownState{byMessage: map[string]string{
		"a@x": "TA", "b@x": "TB", "c@x": "TC",
	}}
	msg := Message{MessageID: "late@x", References: []string{"c@x", "a@x", "b@x"}}

	first := Resolve(msg, known)
	for i := 0; i < 50; i++ {
		again := Resolve(msg, known)
		if again.ThreadID != first.ThreadID || !reflect.DeepEqual(again.MergedFrom, first.MergedFrom) {
			t.Fatalf("run %d placed it differently: %+v then %+v", i, first, again)
		}
	}
	// References order decides, and In-Reply-To is read first: the surviving
	// thread is the one named earliest by the message itself.
	if first.ThreadID != "TC" {
		t.Errorf("surviving thread = %q, want TC (the first reference)", first.ThreadID)
	}
}

// A caller that wants identity-only threading returns false for every subject,
// and gets exactly that. The subject join is a policy, and this pins that it is
// reachable as one rather than baked in.
func TestSubjectJoiningCanBeRefused(t *testing.T) {
	identityOnly := knownState{byMessage: map[string]string{"root@a": "T1"}}
	got := Resolve(Message{MessageID: "x@x", Subject: "Re: project status"}, identityOnly)
	if got.ThreadID != "" {
		t.Errorf("thread = %q with no subject index, want none", got.ThreadID)
	}
}
