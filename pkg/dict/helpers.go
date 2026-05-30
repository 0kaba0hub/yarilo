package dict

import "fmt"

// MemoryTx is a reusable transaction buffer for drivers that lack
// native atomic multi-key writes (file, memory). Drivers embed one of
// these in their concrete Tx type and call Apply at Commit time with
// a per-driver callback that writes the buffered ops.
//
// Mirrors Dovecot's dict-transaction-memory.{c,h} helper, which serves
// the same purpose for dict-file.
type MemoryTx struct {
	Ops []TxOp
}

// TxOpKind is one buffered operation in a MemoryTx.
type TxOpKind int

const (
	OpSet TxOpKind = iota
	OpUnset
	OpAtomicInc
)

// TxOp is one buffered transaction operation. Set carries Value;
// AtomicInc carries Delta; Unset carries neither.
type TxOp struct {
	Kind  TxOpKind
	Key   string
	Value []byte
	Delta int64
}

// Set buffers a Set operation.
func (t *MemoryTx) Set(key string, value []byte) {
	t.Ops = append(t.Ops, TxOp{Kind: OpSet, Key: key, Value: append([]byte(nil), value...)})
}

// Unset buffers an Unset operation.
func (t *MemoryTx) Unset(key string) {
	t.Ops = append(t.Ops, TxOp{Kind: OpUnset, Key: key})
}

// AtomicInc buffers an AtomicInc operation.
func (t *MemoryTx) AtomicInc(key string, delta int64) {
	t.Ops = append(t.Ops, TxOp{Kind: OpAtomicInc, Key: key, Delta: delta})
}

// Reset drops all buffered ops so the MemoryTx can be reused.
func (t *MemoryTx) Reset() {
	t.Ops = t.Ops[:0]
}

// FormatOp returns a short human-readable description of an op,
// for use in error messages and CLI dumps.
func (o TxOp) FormatOp() string {
	switch o.Kind {
	case OpSet:
		return fmt.Sprintf("set %q (%d bytes)", o.Key, len(o.Value))
	case OpUnset:
		return fmt.Sprintf("unset %q", o.Key)
	case OpAtomicInc:
		return fmt.Sprintf("atomic-inc %q %+d", o.Key, o.Delta)
	default:
		return fmt.Sprintf("unknown-op(kind=%d, key=%q)", o.Kind, o.Key)
	}
}

// PathMatches reports whether key falls under path according to the
// dict iteration semantics: when recurse is true, any key starting
// with path matches; otherwise key must lie directly under path with
// no further '/' beyond it. When exactKey is true, only key == path
// matches. Drivers reuse this in their Iterate implementations.
func PathMatches(path, key string, recurse, exactKey bool) bool {
	if exactKey {
		return key == path
	}
	if path == "" {
		// empty path == iterate everything; let recurse control depth.
		if recurse {
			return true
		}
		// Non-recursive iteration of the empty path means top-level
		// keys only (no '/').
		for i := 0; i < len(key); i++ {
			if key[i] == '/' {
				return false
			}
		}
		return true
	}
	if len(key) < len(path) || key[:len(path)] != path {
		return false
	}
	if recurse {
		return true
	}
	// Non-recursive: the remainder beyond path must not contain '/'.
	for i := len(path); i < len(key); i++ {
		if key[i] == '/' {
			return false
		}
	}
	return true
}
