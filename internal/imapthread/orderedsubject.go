package imapthread

import (
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
)

// OrderedSubject implements the ORDEREDSUBJECT algorithm of RFC 5256 §4 --
// "poor man's threading": messages sharing a base subject are one thread,
// oldest first, and the threads themselves are ordered by their first message.
//
// The shape is fixed by the specification: the first message is the root and
// EVERY later message is its child, siblings of one another. There are no
// grandchildren, so a five-message thread is "(1 (2)(3)(4)(5))" and not
// "(1 2 3 4 5)" -- the latter would claim each message answers the one before
// it, which is precisely the claim this algorithm cannot make.
func OrderedSubject(msgs []Message) []imaplib.ThreadNode {
	type thread struct {
		msgs []*Message
	}
	var order []string
	threads := map[string]*thread{}
	for i := range msgs {
		m := &msgs[i]
		base, _ := BaseSubject(m.Subject)
		// Subject comparisons are case-insensitive (§5, Internationalization
		// Considerations).
		key := strings.ToLower(base)
		t, ok := threads[key]
		if !ok {
			t = &thread{}
			threads[key] = t
			order = append(order, key)
		}
		t.msgs = append(t.msgs, m)
	}

	// The threads are ordered by their own first message, so the node and that
	// message travel together: sorting the nodes while looking their dates up
	// by position would read the key of whatever landed there instead.
	type built struct {
		node  imaplib.ThreadNode
		first *Message
	}
	list := make([]built, 0, len(threads))
	for _, key := range order {
		t := threads[key]
		sort.SliceStable(t.msgs, func(i, j int) bool { return earlier(t.msgs[i], t.msgs[j]) })
		node := imaplib.ThreadNode{Num: t.msgs[0].Num}
		for _, child := range t.msgs[1:] {
			node.Children = append(node.Children, imaplib.ThreadNode{Num: child.Num})
		}
		list = append(list, built{node: node, first: t.msgs[0]})
	}
	sort.SliceStable(list, func(i, j int) bool { return earlier(list[i].first, list[j].first) })

	out := make([]imaplib.ThreadNode, 0, len(list))
	for _, b := range list {
		out = append(out, b.node)
	}
	return out
}

func earlier(a, b *Message) bool {
	if !a.Sent.Equal(b.Sent) {
		return a.Sent.Before(b.Sent)
	}
	return a.Num < b.Num
}
