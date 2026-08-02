package director

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

func TestBackendSetHash_OrderIndependentAndFieldScoped(t *testing.T) {
	base := []ring.Backend{
		{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true, Vhosts: 100},
		{IP: "10.0.0.2", Port: 993, Tag: "imap", Up: true, Vhosts: 100},
	}
	want := backendSetHash(base)

	tests := []struct {
		name string
		in   []ring.Backend
		same bool
	}{
		{
			name: "reordered set hashes identically",
			in:   []ring.Backend{base[1], base[0]},
			same: true,
		},
		{
			name: "vhosts 0 == default 100",
			in: []ring.Backend{
				{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true, Vhosts: 0},
				{IP: "10.0.0.2", Port: 993, Tag: "imap", Up: true, Vhosts: 100},
			},
			same: true,
		},
		{
			name: "transient LastUp/LastDown/Hostname excluded",
			in: []ring.Backend{
				{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true, Vhosts: 100, LastUp: 12345, Hostname: "pod-a"},
				{IP: "10.0.0.2", Port: 993, Tag: "imap", Up: true, Vhosts: 100, LastDown: 99},
			},
			same: true,
		},
		{
			name: "up flip changes the hash",
			in: []ring.Backend{
				{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: false, Vhosts: 100},
				{IP: "10.0.0.2", Port: 993, Tag: "imap", Up: true, Vhosts: 100},
			},
			same: false,
		},
		{
			name: "vhosts weight change changes the hash",
			in: []ring.Backend{
				{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true, Vhosts: 200},
				{IP: "10.0.0.2", Port: 993, Tag: "imap", Up: true, Vhosts: 100},
			},
			same: false,
		},
		{
			name: "missing backend changes the hash",
			in:   base[:1],
			same: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := backendSetHash(tc.in)
			if tc.same && got != want {
				t.Errorf("hash = %s, want same as %s", got, want)
			}
			if !tc.same && got == want {
				t.Errorf("hash = %s, expected to differ from %s", got, want)
			}
		})
	}
}

func TestBackendSetHash_EmptySetStable(t *testing.T) {
	if backendSetHash(nil) != backendSetHash([]ring.Backend{}) {
		t.Error("nil and empty backend sets must hash identically")
	}
}
