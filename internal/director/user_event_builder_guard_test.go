package director

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A user event that reaches the ring through the generic originate carries the
// username raw: a name with a TAB then shifts every field after it, and the
// receiving side reads a fragment of the name as a backend address. One
// builder escapes it (#1365), and this guard is what keeps a later event from
// quietly going around that builder -- the same trick as loginKickLine in
// #1364, where two call sites drifted apart and nothing noticed for months.
//
// Read from the source rather than from behaviour, because the failure it
// guards is a call site that does not exist yet.
func TestUserEventsOnlyLeaveThroughTheEscapingBuilder(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// The builder itself, and the reader's normalisation, name these kinds on
	// purpose; every other mention in a call is a raw send.
	allowed := map[string]bool{
		"membership.go": true, // originateUserEvent and the kind tables
	}

	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(".", name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for plain := range plainToEscapedUserEvent {
				// A user event handed to a generic sender: the username in
				// that payload was never escaped.
				if strings.Contains(trimmed, `originate("`+plain+`"`) ||
					strings.Contains(trimmed, `originateRingEvent("`+plain+`"`) {
					offenders = append(offenders, name+":"+itoa(i+1)+": "+trimmed)
				}
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these send a user event without escaping the username; use originateUserEvent:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
