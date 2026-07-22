package language

import "strings"

// atext per RFC 5322 (the address tokenizer's localpart alphabet).
var atextClass = [256]bool{}

func init() {
	for c := 'A'; c <= 'Z'; c++ {
		atextClass[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		atextClass[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		atextClass[c] = true
	}
	for _, c := range "!#$%&'*+-/=?^_`{|}~" {
		atextClass[c] = true
	}
}

func isDomainByte(c byte) bool {
	return atextClass[c] || c == '.'
}

// Address is the streaming e-mail-address tokenizer layered over Generic,
// following the reference address-tokenizer semantics: a complete
// localpart@domain is emitted
// as ONE token (up to maxLen bytes). In index mode all input additionally
// flows through the parent Generic so the address parts are indexed too; in
// search mode a complete address is withheld from the parent so a query for
// an address matches only the whole-address token.
type Address struct {
	parent *Generic
	maxLen int
	search bool

	state addrState
	word  []byte // candidate localpart[@domain] being collected
	hold  []byte // search mode: input withheld from the parent
}

type addrState int

const (
	addrNone addrState = iota
	addrLocalpart
	addrDomain
)

// NewAddress wraps parent. search selects the query-time behaviour.
func NewAddress(parent *Generic, maxLen int, search bool) *Address {
	if maxLen <= 0 {
		maxLen = DefaultAddressMaxLen
	}
	return &Address{parent: parent, maxLen: maxLen, search: search}
}

func (a *Address) emitAddress(emit EmitFunc) error {
	// Trailing '.' and '-' are stripped (#725 items 1-2): both are valid
	// mid-domain atext/label bytes the collector accumulates, but neither
	// can trail a real domain, mirroring the reference's own trimming.
	addr := strings.TrimRight(string(a.word), ".-")
	a.word = a.word[:0]
	a.state = addrNone
	at := strings.IndexByte(addr, '@')
	// at < 0: no '@' at all. at == len(addr)-1: '@' is the last byte, i.e.
	// an empty domain after trimming (e.g. "user@", "user@-") — the
	// reference drops these as phantom tokens, not real addresses.
	if at < 0 || at == len(addr)-1 || len(addr) > a.maxLen {
		return nil
	}
	return emit(addr)
}

// feedParent routes input to the parent tokenizer. In search mode the bytes
// of an in-progress candidate are buffered so a completed address can be
// withheld so an address query matches only the whole-address token.
func (a *Address) feedParent(b []byte, emit EmitFunc) error {
	if a.search {
		a.hold = append(a.hold, b...)
		return nil
	}
	return a.parent.Feed(b, emit)
}

func (a *Address) flushHold(emit EmitFunc, dropLast int) error {
	if !a.search {
		return nil
	}
	h := a.hold
	if dropLast > 0 && dropLast <= len(h) {
		h = h[:len(h)-dropLast]
	}
	a.hold = a.hold[:0]
	if len(h) == 0 {
		return nil
	}
	return a.parent.Feed(h, emit)
}

// Feed streams data. emit receives both whole-address tokens and the parent
// tokenizer's word tokens.
func (a *Address) Feed(data []byte, emit EmitFunc) error {
	for i := 0; i < len(data); i++ {
		c := data[i]
		var complete bool
		switch a.state {
		case addrNone:
			if atextClass[c] || c == '.' {
				a.state = addrLocalpart
				a.word = append(a.word, c)
			}
		case addrLocalpart:
			switch {
			case atextClass[c] || c == '.':
				a.word = append(a.word, c)
			case c == '@':
				a.state = addrDomain
				a.word = append(a.word, c)
			default:
				a.word = a.word[:0]
				a.state = addrNone
			}
		case addrDomain:
			if isDomainByte(c) {
				a.word = append(a.word, c)
			} else {
				complete = true
			}
		}
		if err := a.feedParent(data[i:i+1], emit); err != nil {
			return err
		}
		if complete {
			wordLen := len(a.word)
			if err := a.emitAddress(emit); err != nil {
				return err
			}
			// The withheld buffer ends with the address bytes plus the
			// terminator byte just fed; keep the terminator, drop the address.
			if err := a.flushHold(emit, func() int {
				if wordLen+1 <= len(a.hold) {
					return wordLen + 1
				}
				return len(a.hold)
			}()); err != nil {
				return err
			}
			if a.search {
				// Re-feed the terminator so the parent still sees the break.
				if err := a.parent.Feed(data[i:i+1], emit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Flush finalises input: a candidate still in the domain state is a complete
// address at end of data.
func (a *Address) Flush(emit EmitFunc) error {
	if a.state == addrDomain {
		wordLen := len(a.word)
		if err := a.emitAddress(emit); err != nil {
			return err
		}
		if err := a.flushHold(emit, wordLen); err != nil {
			return err
		}
	} else {
		a.word = a.word[:0]
		a.state = addrNone
		if err := a.flushHold(emit, 0); err != nil {
			return err
		}
	}
	return a.parent.Flush(emit)
}
