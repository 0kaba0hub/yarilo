package imapthread

import (
	"fmt"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// render writes the trees the way RFC 5256 §4 prints them, so a failure reads
// as the specification's own notation rather than as a Go struct dump.
func render(nodes []imaplib.ThreadNode) string {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString("(")
		renderChain(&b, n, true)
		b.WriteString(")")
	}
	return b.String()
}

func renderChain(b *strings.Builder, n imaplib.ThreadNode, first bool) {
	for {
		if n.Num != 0 {
			if !first {
				b.WriteString(" ")
			}
			fmt.Fprintf(b, "%d", n.Num)
			first = false
		}
		switch len(n.Children) {
		case 0:
			return
		case 1:
			n = n.Children[0]
		default:
			for _, c := range n.Children {
				if !first {
					b.WriteString(" ")
					first = true
				}
				b.WriteString("(")
				renderChain(b, c, true)
				b.WriteString(")")
			}
			return
		}
	}
}

// msg builds a message whose sent date follows its number, so that ordering is
// the mailbox order unless a row says otherwise.
func msg(num uint32, id, subject string, refs ...string) Message {
	return Message{
		Num:        num,
		MessageID:  "<" + id + ">",
		References: refs,
		Subject:    subject,
		Sent:       time.Date(2026, 3, 1, 0, 0, int(num), 0, time.UTC),
	}
}

func TestReferencesBuildsTheTreesOfRFC5256(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want string
	}{{
		name: "unrelated messages are each their own thread",
		msgs: []Message{msg(1, "a@x", "One"), msg(2, "b@x", "Two")},
		want: "(1)(2)",
	}, {
		name: "a reply is a child of what it answers",
		msgs: []Message{msg(1, "a@x", "One"), msg(2, "b@x", "Re: One", "<a@x>")},
		want: "(1 2)",
	}, {
		// Two answers to one message branch; flattening this would say 3
		// answers 2.
		name: "two replies to the same message branch",
		msgs: []Message{
			msg(1, "a@x", "One"),
			msg(2, "b@x", "Re: One", "<a@x>"),
			msg(3, "c@x", "Re: One", "<a@x>"),
		},
		want: "(1 (2)(3))",
	}, {
		// §4 step 3: the parent was never delivered, so the dummy holding its
		// two replies together stays. "(2 3)" would claim 3 answers 2.
		name: "replies whose parent is missing stay grouped under a dummy",
		msgs: []Message{
			msg(2, "b@x", "Re: One", "<missing@x>"),
			msg(3, "c@x", "Re: One", "<missing@x>"),
		},
		want: "((2)(3))",
	}, {
		// The same, with the subjects differing so that step 5 cannot regroup
		// them: this row is what actually pins step 3's rule. With equal
		// subjects the two steps produce the same tree by different routes,
		// and the row would pass with the rule deleted.
		name: "the dummy survives even when subjects differ",
		msgs: []Message{
			msg(2, "b@x", "Budget", "<missing@x>"),
			msg(3, "c@x", "Timeline", "<missing@x>"),
		},
		want: "((2)(3))",
	}, {
		// The same, with one reply: a dummy with a single child is pruned
		// (step 3), because nothing is being held together.
		name: "one reply to a missing parent is a thread of its own",
		msgs: []Message{msg(2, "b@x", "Re: One", "<missing@x>")},
		want: "(2)",
	}, {
		// Step 1.A: the References chain names ancestors the mailbox does not
		// hold, and their dummies carry the descent.
		name: "a chain through absent ancestors",
		msgs: []Message{
			msg(1, "a@x", "Deep", "<g1@x>", "<g2@x>"),
			msg(2, "b@x", "Re: Deep", "<g1@x>", "<g2@x>", "<a@x>"),
		},
		want: "(1 2)",
	}, {
		// Step 5: a reply that lost its References header still joins its
		// conversation by base subject -- and becomes a child, because it is
		// the reply and the other is not.
		name: "a reply with no references joins by subject",
		msgs: []Message{msg(1, "a@x", "One"), msg(2, "b@x", "Re: One")},
		want: "(1 2)",
	}, {
		// Step 5, the last rule: two originals with one subject and no
		// ancestry between them. Neither may be made the other's parent, so a
		// dummy holds both.
		name: "two originals sharing a subject get a dummy parent",
		msgs: []Message{msg(1, "a@x", "One"), msg(2, "b@x", "One")},
		want: "((1)(2))",
	}, {
		// Step 5.B.ii: an empty subject groups nothing. Otherwise every
		// subjectless message in a mailbox would become one conversation.
		name: "empty subjects do not group",
		msgs: []Message{msg(1, "a@x", ""), msg(2, "b@x", "")},
		want: "(1)(2)",
	}, {
		// Step 1: two messages claiming one Message-ID are two messages. Only
		// the first keeps the id.
		name: "a duplicate message id does not merge two messages",
		msgs: []Message{msg(1, "same@x", "One"), msg(2, "same@x", "Two")},
		want: "(1)(2)",
	}, {
		// A message that references itself, directly or through a cycle, must
		// not disappear into a loop.
		name: "a self-reference is not a loop",
		msgs: []Message{msg(1, "a@x", "One", "<a@x>")},
		want: "(1)",
	}, {
		// §2.2: order is the sent date, not the mailbox position.
		name: "threads are ordered by sent date",
		msgs: []Message{
			{Num: 1, MessageID: "<a@x>", Subject: "Later", Sent: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)},
			{Num: 2, MessageID: "<b@x>", Subject: "Earlier", Sent: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		},
		want: "(2)(1)",
	}, {
		name: "quoting in a message id is not a difference",
		msgs: []Message{
			msg(1, `"a"@x`, "One"),
			msg(2, "b@x", "Re: One", "<a@x>"),
		},
		want: "(1 2)",
	}, {
		name: "no messages",
		msgs: nil,
		want: "",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(References(tc.msgs)); got != tc.want {
				t.Errorf("References() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOrderedSubjectGroupsBySubjectAlone(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want string
	}{{
		// §4: the first message is the root and every later one is its child,
		// siblings of each other. There are no grandchildren, so this is not
		// "(1 2 3)" -- that would say 3 answers 2.
		name: "one subject, three messages",
		msgs: []Message{msg(1, "a@x", "Plan"), msg(2, "b@x", "Re: Plan"), msg(3, "c@x", "Re: Plan")},
		want: "(1 (2)(3))",
	}, {
		name: "two messages are a parent and a child",
		msgs: []Message{msg(1, "a@x", "Plan"), msg(2, "b@x", "Re: Plan")},
		want: "(1 2)",
	}, {
		// References are not consulted at all: this algorithm knows only
		// subjects, which is why it is called poor man's threading.
		name: "ancestry is ignored",
		msgs: []Message{
			msg(1, "a@x", "Plan"),
			msg(2, "b@x", "Something else", "<a@x>"),
		},
		want: "(1)(2)",
	}, {
		name: "subject comparison is case-insensitive",
		msgs: []Message{msg(1, "a@x", "Plan"), msg(2, "b@x", "RE: PLAN")},
		want: "(1 2)",
	}, {
		name: "threads are ordered by their first message",
		msgs: []Message{
			{Num: 1, MessageID: "<a@x>", Subject: "Later", Sent: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)},
			{Num: 2, MessageID: "<b@x>", Subject: "Earlier", Sent: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		},
		want: "(2)(1)",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(OrderedSubject(tc.msgs)); got != tc.want {
				t.Errorf("OrderedSubject() = %s, want %s", got, tc.want)
			}
		})
	}
}
