package dict

import (
	"context"
	"errors"
	"testing"
)

func TestEscapeUnescape(t *testing.T) {
	cases := []struct {
		name, in, escaped string
	}{
		{"plain", "hello", "hello"},
		{"slash", "a/b", "a%2fb"},
		{"percent", "50%", "50%25"},
		{"both", "a/b%c/d", "a%2fb%25c%2fd"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Escape(tc.in)
			if got != tc.escaped {
				t.Errorf("Escape(%q) = %q, want %q", tc.in, got, tc.escaped)
			}
			roundTrip := Unescape(got)
			if roundTrip != tc.in {
				t.Errorf("Unescape(Escape(%q)) = %q, want %q", tc.in, roundTrip, tc.in)
			}
		})
	}
}

func TestUnescapeUppercaseHex(t *testing.T) {
	if got := Unescape("a%2Fb"); got != "a/b" {
		t.Errorf("Unescape uppercase hex: got %q, want %q", got, "a/b")
	}
}

func TestUnescapeInvalidPassthrough(t *testing.T) {
	cases := map[string]string{
		"%XY": "%XY", // not a hex pair we recognise
		"50%": "50%", // trailing percent
		"%2":  "%2",  // truncated
	}
	for in, want := range cases {
		if got := Unescape(in); got != want {
			t.Errorf("Unescape(%q) = %q, want %q (invalid sequences pass through)", in, got, want)
		}
	}
}

func TestPathMatches(t *testing.T) {
	cases := []struct {
		name           string
		path, key      string
		recurse, exact bool
		want           bool
	}{
		{"exact hit", "priv/box/INBOX/comment", "priv/box/INBOX/comment", false, true, true},
		{"exact miss", "priv/box/INBOX/comment", "priv/box/INBOX/other", false, true, false},

		{"prefix recurse hit", "priv/box/INBOX/", "priv/box/INBOX/sub/key", true, false, true},
		{"prefix shallow miss", "priv/box/INBOX/", "priv/box/INBOX/sub/key", false, false, false},
		{"prefix shallow hit", "priv/box/INBOX/", "priv/box/INBOX/comment", false, false, true},

		{"prefix not under path", "priv/box/INBOX/", "priv/box/OTHER/x", true, false, false},
		{"prefix not under path shallow", "priv/box/INBOX/", "shared/x", false, false, false},

		{"empty path recurse", "", "anything/at/all", true, false, true},
		{"empty path shallow only top", "", "top", false, false, true},
		{"empty path shallow rejects nested", "", "top/nested", false, false, false},

		{"prefix exactly equal recurse", "priv/", "priv/", true, false, true},
		{"prefix exactly equal shallow", "priv/", "priv/", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PathMatches(tc.path, tc.key, tc.recurse, tc.exact)
			if got != tc.want {
				t.Errorf("PathMatches(%q, %q, recurse=%v, exact=%v) = %v, want %v",
					tc.path, tc.key, tc.recurse, tc.exact, got, tc.want)
			}
		})
	}
}

func TestMemoryTxBuffersOps(t *testing.T) {
	var tx MemoryTx
	tx.Set("a", []byte("one"))
	tx.Unset("b")
	tx.AtomicInc("c", 7)
	if len(tx.Ops) != 3 {
		t.Fatalf("expected 3 buffered ops, got %d", len(tx.Ops))
	}
	if tx.Ops[0].Kind != OpSet || tx.Ops[0].Key != "a" || string(tx.Ops[0].Value) != "one" {
		t.Errorf("op[0] = %+v", tx.Ops[0])
	}
	if tx.Ops[1].Kind != OpUnset || tx.Ops[1].Key != "b" {
		t.Errorf("op[1] = %+v", tx.Ops[1])
	}
	if tx.Ops[2].Kind != OpAtomicInc || tx.Ops[2].Key != "c" || tx.Ops[2].Delta != 7 {
		t.Errorf("op[2] = %+v", tx.Ops[2])
	}

	// Value is defensively copied.
	v := []byte("seven")
	tx.Set("d", v)
	v[0] = 'X'
	if string(tx.Ops[3].Value) != "seven" {
		t.Errorf("Set must defensively copy value, got %q after caller mutation", tx.Ops[3].Value)
	}

	tx.Reset()
	if len(tx.Ops) != 0 {
		t.Errorf("Reset did not clear ops; len=%d", len(tx.Ops))
	}
}

func TestOpenUnknownDriver(t *testing.T) {
	_, err := Open(Config{Driver: "no-such-driver-zzz"})
	if !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("Open with unknown driver: err = %v, want ErrUnknownDriver", err)
	}
}

func TestOpenEmptyDriver(t *testing.T) {
	_, err := Open(Config{})
	if err == nil {
		t.Fatal("Open with empty driver should error")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	const name = "test-dup-driver-zzz"
	noop := func(_ Config) (Dict, error) { return nil, nil }
	Register(name, noop)
	defer func() {
		// Unregister via internal map mutation — only for this test.
		driversMu.Lock()
		delete(drivers, name)
		driversMu.Unlock()
	}()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register with duplicate name must panic")
		}
	}()
	Register(name, noop)
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register with empty name must panic")
		}
	}()
	Register("", func(_ Config) (Dict, error) { return nil, nil })
}

func TestRegisterNilInitPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register with nil init must panic")
		}
	}()
	Register("test-nil-init-zzz", nil)
}

// Compile-time guard: ensure our context import is exercised so the
// file is self-contained for go vet even before drivers exist.
var _ context.Context = context.Background()
