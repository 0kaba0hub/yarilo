package mailbox

import (
	"errors"
	"testing"
)

type fakeHealer struct{}

func (fakeHealer) HealCorruptFolder(UserIndex, *Folder) (int, error) { return 0, nil }

type plainBox struct{}

func TestCanReactiveHeal(t *testing.T) {
	if !CanReactiveHeal(fakeHealer{}) {
		t.Error("a ReactiveHealer must qualify")
	}
	if CanReactiveHeal(plainBox{}) {
		t.Error("a driver without HealCorruptFolder must not qualify")
	}
	if CanReactiveHeal(nil) {
		t.Error("nil must not qualify")
	}
}

// TestMarkCorruptOnFetchErrGating verifies the marker is gated: every no-op path
// must return before touching idx. idx is nil here, so any path that reached the
// idx dereference would panic — reaching the end is the assertion.
func TestMarkCorruptOnFetchErrGating(t *testing.T) {
	// Box cannot heal → must not mark (would otherwise strand a folder FSCKD).
	MarkCorruptOnFetchErr(plainBox{}, nil, "INBOX", ErrCorruptStorage)
	// err is nil → no-op.
	MarkCorruptOnFetchErr(fakeHealer{}, nil, "INBOX", nil)
	// err is not corruption → no-op (transient I/O must not mark).
	MarkCorruptOnFetchErr(fakeHealer{}, nil, "INBOX", errors.New("input/output error"))
}
