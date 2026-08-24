package imapthread

import (
	"fmt"
	"sort"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// Message is one searched message, as the threading algorithms need to see it.
//
// Num is what the reply will carry -- a sequence number or a UID, decided by
// the command, not by this package. Sent is §2.2's sent date: the Date header
// in UTC, or the internal date when there is none to parse.
type Message struct {
	Num        uint32
	MessageID  string
	References []string
	Subject    string
	Sent       time.Time
}

// container is a node while the tree is being built. RFC 5256 calls a
// container with no message a "dummy": a message the thread refers to but the
// mailbox does not hold.
type container struct {
	msg      *Message
	parent   *container
	children []*container

	base  string
	refwd bool
}

func (c *container) dummy() bool { return c.msg == nil }

func (c *container) descends(from *container) bool {
	for p := c; p != nil; p = p.parent {
		if p == from {
			return true
		}
	}
	return false
}

// link makes parent the parent of child, obeying the two rules step 1 states:
// an existing parent is not replaced, and no link may close a loop.
func link(parent, child *container) {
	if child.parent != nil || parent == child || parent.descends(child) {
		return
	}
	parent.adopt(child)
}

func (c *container) adopt(child *container) {
	child.detach()
	child.parent = c
	c.children = append(c.children, child)
}

func (c *container) detach() {
	if c.parent == nil {
		return
	}
	siblings := c.parent.children
	for i, s := range siblings {
		if s == c {
			c.parent.children = append(siblings[:i:i], siblings[i+1:]...)
			break
		}
	}
	c.parent = nil
}

// References implements the REFERENCES threading algorithm of RFC 5256 §4.
func References(msgs []Message) []imaplib.ThreadNode {
	byID := map[string]*container{}
	// Step 1: link every message to its ancestry.
	for i := range msgs {
		m := &msgs[i]
		self := containerFor(byID, messageIDOf(byID, m, i))
		self.msg = m
		self.base, self.refwd = BaseSubject(m.Subject)

		// (A) The references are a chain: each is the parent of the next.
		var prev *container
		for _, ref := range m.References {
			ref = normalizeMessageID(ref)
			if ref == "" {
				continue
			}
			cur := containerFor(byID, ref)
			if prev != nil {
				link(prev, cur)
			}
			prev = cur
		}
		// (B) The last reference is this message's parent. Here an existing
		// link IS broken: a truncated References header is exactly what gives
		// a message the wrong parent, and this is the correct one.
		if prev != nil && prev != self && !prev.descends(self) {
			self.detach()
			prev.adopt(self)
		}
	}

	// Step 2: everything without a parent hangs off one root.
	root := &container{}
	for _, c := range sortedContainers(byID) {
		if c.parent == nil && c != root {
			root.adopt(c)
		}
	}

	// Step 3: dummies exist to hold a tree together; the ones holding nothing
	// go.
	pruneDummies(root, root)

	// Step 4 and 5 both work on the top level, and 5 compares sent dates of
	// what 4 ordered.
	sortSiblings(root)
	gatherBySubject(root)
	// Step 6: sort every generation, children before their parents, so a
	// parent orders by a child list that is already ordered.
	sortTree(root)

	return nodesOf(root.children)
}

// containerFor is the id table of step 1: one container per Message ID,
// created on first mention, so a reference to a message the mailbox does not
// hold becomes the dummy the specification asks for.
func containerFor(byID map[string]*container, id string) *container {
	c, ok := byID[id]
	if !ok {
		c = &container{}
		byID[id] = c
	}
	return c
}

// messageIDOf answers what identity this message threads under: its own, or a
// unique one when it has none or when an earlier message already claimed it.
// Two unrelated messages that share a Message-ID -- a resend, a broken mailer
// -- must not become one conversation.
func messageIDOf(byID map[string]*container, m *Message, index int) string {
	id := normalizeMessageID(m.MessageID)
	if id == "" {
		return fmt.Sprintf("\x00no-message-id-%d", index)
	}
	if c, taken := byID[id]; taken && c.msg != nil {
		return fmt.Sprintf("\x00duplicate-%s-%d", id, index)
	}
	return id
}

// normalizeMessageID strips the angle brackets and the quoting RFC 5256
// requires be ignored: <"a"@b> and <a@b> are the same identity. Comparison
// stays case-sensitive, as the specification notes.
func normalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.ReplaceAll(id, `"`, "")
}

// sortedContainers gives step 2 a deterministic order: map iteration would
// otherwise let two runs over one mailbox answer differently.
func sortedContainers(byID map[string]*container) []*container {
	out := make([]*container, 0, len(byID))
	for _, c := range byID {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

// pruneDummies implements step 3.
func pruneDummies(root, c *container) {
	for _, child := range append([]*container(nil), c.children...) {
		pruneDummies(root, child)
	}
	if !c.dummy() || c == root {
		return
	}
	if len(c.children) == 0 {
		c.detach()
		return
	}
	// A dummy with several children stays when its children would otherwise
	// become top-level threads: it is the only thing recording that they
	// answer the same absent message.
	if c.parent == root && len(c.children) > 1 {
		return
	}
	parent := c.parent
	for _, child := range append([]*container(nil), c.children...) {
		parent.adopt(child)
	}
	c.detach()
}

// gatherBySubject implements step 5: threads whose first message shares a base
// subject are one conversation, which is how a reply that lost its References
// header still finds its thread.
func gatherBySubject(root *container) {
	table := map[string]*container{}
	for _, c := range root.children {
		subject, _ := threadSubject(c)
		if subject == "" {
			continue
		}
		prev, seen := table[subject]
		if !seen {
			table[subject] = c
			continue
		}
		// The best thread head for a subject is a dummy (it holds replies
		// without claiming to be their parent), then an original, then a
		// reply.
		_, prevRefwd := threadSubject(prev)
		_, curRefwd := threadSubject(c)
		if !prev.dummy() && (c.dummy() || (prevRefwd && !curRefwd)) {
			table[subject] = c
		}
	}

	for _, c := range append([]*container(nil), root.children...) {
		subject, refwd := threadSubject(c)
		if subject == "" {
			continue
		}
		head := table[subject]
		if head == nil || head == c {
			continue
		}
		_, headRefwd := threadSubject(head)
		switch {
		case head.dummy() && c.dummy():
			for _, child := range append([]*container(nil), c.children...) {
				head.adopt(child)
			}
			c.detach()
		case head.dummy():
			head.adopt(c)
		case refwd && !headRefwd:
			head.adopt(c)
		default:
			// Neither is obviously the other's parent, so neither is made one:
			// a new dummy holds both as siblings.
			merged := &container{}
			root.adopt(merged)
			merged.adopt(head)
			merged.adopt(c)
			table[subject] = merged
		}
	}
}

// threadSubject is step 5.B.i: a dummy has no subject of its own, so the
// thread is named by its first child.
func threadSubject(c *container) (string, bool) {
	if !c.dummy() {
		return strings.ToLower(c.base), c.refwd
	}
	if len(c.children) == 0 {
		return "", false
	}
	return strings.ToLower(c.children[0].base), c.children[0].refwd
}

func sortSiblings(c *container) {
	sort.SliceStable(c.children, func(i, j int) bool { return less(c.children[i], c.children[j]) })
}

// sortTree sorts the youngest generation first, as step 6 requires: a dummy
// sorts by its first child, so that child list must already be in order.
func sortTree(c *container) {
	for _, child := range c.children {
		sortTree(child)
	}
	sortSiblings(c)
}

// less orders by sent date, and by mailbox position when the dates match
// exactly (§2.2). A dummy is ordered by its first child, which is the only
// date it has.
func less(a, b *container) bool {
	at, an := sortKey(a)
	bt, bn := sortKey(b)
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return an < bn
}

func sortKey(c *container) (time.Time, uint32) {
	if !c.dummy() {
		return c.msg.Sent, c.msg.Num
	}
	if len(c.children) == 0 {
		return time.Time{}, 0
	}
	return sortKey(c.children[0])
}

func nodesOf(cs []*container) []imaplib.ThreadNode {
	out := make([]imaplib.ThreadNode, 0, len(cs))
	for _, c := range cs {
		node := imaplib.ThreadNode{Children: nodesOf(c.children)}
		if !c.dummy() {
			node.Num = c.msg.Num
		}
		out = append(out, node)
	}
	return out
}
