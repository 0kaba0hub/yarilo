package ring

import (
	"fmt"
	"testing"
)

func TestLookup_EmptyRing(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	if got := r.Lookup("alice@example.com"); got != "" {
		t.Fatalf("empty ring: want \"\", got %q", got)
	}
}

func TestLookup_SingleBackend(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Up: true})
	got := r.Lookup("alice@example.com")
	if got != "10.0.0.1" {
		t.Fatalf("single backend: want 10.0.0.1, got %q", got)
	}
}

func TestLookup_DownBackendExcluded(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Up: false})
	r.AddBackend(&Backend{IP: "10.0.0.2", Up: true})
	got := r.Lookup("alice@example.com")
	if got != "10.0.0.2" {
		t.Fatalf("down backend must be excluded: got %q", got)
	}
}

func TestLookup_Consistency(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	for i := 1; i <= 3; i++ {
		r.AddBackend(&Backend{IP: fmt.Sprintf("10.0.0.%d", i), Up: true})
	}
	// Same username must always map to the same backend.
	first := r.Lookup("bob@example.com")
	for i := 0; i < 100; i++ {
		if got := r.Lookup("bob@example.com"); got != first {
			t.Fatalf("inconsistent: iteration %d got %q, want %q", i, got, first)
		}
	}
}

func TestLookup_Distribution(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	backends := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	for _, ip := range backends {
		r.AddBackend(&Backend{IP: ip, Up: true})
	}
	counts := make(map[string]int)
	for i := 0; i < 3000; i++ {
		b := r.Lookup(fmt.Sprintf("user%d@example.com", i))
		counts[b]++
	}
	// Each backend should serve at least 5% of users (very loose bound).
	for _, ip := range backends {
		if counts[ip] < 50 {
			t.Errorf("backend %s got only %d/3000 users — distribution looks wrong", ip, counts[ip])
		}
	}
}

func TestLookupBackendByTag_IsolatesPool(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Tag: "ssd", Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.2", Tag: "ssd", Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.3", Tag: "hdd", Up: true})

	for i := 0; i < 300; i++ {
		b := r.LookupBackendByTag(fmt.Sprintf("user%d@example.com", i), "hdd")
		if b == nil {
			t.Fatal("LookupBackendByTag(hdd): got nil")
		}
		if b.IP != "10.0.0.3" {
			t.Errorf("user%d routed to %q, want 10.0.0.3 (only hdd backend)", i, b.IP)
		}
	}
}

func TestLookupBackendByTag_EmptyTag_UntaggedOnly(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Tag: "ssd", Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.2", Tag: "", Up: true}) // untagged

	b := r.LookupBackendByTag("alice@example.com", "")
	if b == nil {
		t.Fatal("empty tag must route to untagged backends, got nil")
	}
	if b.IP != "10.0.0.2" {
		t.Errorf("empty tag: expected untagged backend 10.0.0.2, got %q", b.IP)
	}
}

func TestLookupBackendByTag_EmptyTag_NilWhenNoUntagged(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Tag: "ssd", Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.2", Tag: "hdd", Up: true})

	b := r.LookupBackendByTag("alice@example.com", "")
	if b != nil {
		t.Errorf("empty tag with no untagged backends: want nil, got %+v", b)
	}
}

func TestLookupBackendByTag_UnknownTag_Nil(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Tag: "ssd", Up: true})

	b := r.LookupBackendByTag("alice@example.com", "nonexistent")
	if b != nil {
		t.Errorf("unknown tag: want nil, got %+v", b)
	}
}

func TestLookupBackendByTag_ConsistentWithinTag(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	for i := 1; i <= 3; i++ {
		r.AddBackend(&Backend{IP: fmt.Sprintf("10.0.0.%d", i), Tag: "ssd", Up: true})
	}
	r.AddBackend(&Backend{IP: "10.1.0.1", Tag: "hdd", Up: true})

	first := r.LookupBackendByTag("bob@example.com", "ssd")
	if first == nil {
		t.Fatal("got nil")
	}
	for i := 0; i < 50; i++ {
		b := r.LookupBackendByTag("bob@example.com", "ssd")
		if b == nil || b.IP != first.IP {
			t.Fatalf("inconsistent: iter %d got %v, want %q", i, b, first.IP)
		}
	}
}

// TestAddBackend_PreservesTransitionMetadata guards #705: re-adding a known
// backend with only LastUp set (a BACKEND-UP heartbeat / admin add) must not
// clobber the existing LastDown / Hostname / LastUp — otherwise the
// timestamp-based peer up/down merge is corrupted.
func TestAddBackend_PreservesTransitionMetadata(t *testing.T) {
	tests := []struct {
		name         string
		existing     Backend
		incoming     Backend
		wantLastUp   int64
		wantLastDown int64
		wantHostname string
	}{
		{
			name:         "heartbeat preserves LastDown and Hostname",
			existing:     Backend{IP: "10.0.0.1", Port: 143, Up: false, LastUp: 100, LastDown: 200, Hostname: "be-1"},
			incoming:     Backend{IP: "10.0.0.1", Port: 143, Up: true, LastUp: 300},
			wantLastUp:   300,
			wantLastDown: 200,
			wantHostname: "be-1",
		},
		{
			name:         "handshake carrying non-zero fields overwrites",
			existing:     Backend{IP: "10.0.0.2", LastUp: 100, LastDown: 200, Hostname: "old"},
			incoming:     Backend{IP: "10.0.0.2", LastUp: 400, LastDown: 350, Hostname: "new"},
			wantLastUp:   400,
			wantLastDown: 350,
			wantHostname: "new",
		},
		{
			name:         "zero LastUp on incoming is preserved from existing",
			existing:     Backend{IP: "10.0.0.3", LastUp: 500, LastDown: 0, Hostname: "h"},
			incoming:     Backend{IP: "10.0.0.3", Up: true},
			wantLastUp:   500,
			wantLastDown: 0,
			wantHostname: "h",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(MustParseHashFormat("%u"))
			ex := tt.existing
			r.AddBackend(&ex)
			in := tt.incoming
			r.AddBackend(&in)
			got := r.GetBackend(tt.incoming.IP)
			if got == nil {
				t.Fatal("backend missing after re-add")
			}
			if got.LastUp != tt.wantLastUp {
				t.Errorf("LastUp = %d, want %d", got.LastUp, tt.wantLastUp)
			}
			if got.LastDown != tt.wantLastDown {
				t.Errorf("LastDown = %d, want %d", got.LastDown, tt.wantLastDown)
			}
			if got.Hostname != tt.wantHostname {
				t.Errorf("Hostname = %q, want %q", got.Hostname, tt.wantHostname)
			}
		})
	}
}

func TestTags(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Tag: "ssd", Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.2", Tag: "hdd", Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.3", Tag: "ssd", Up: true})

	tags := r.Tags()
	if len(tags) != 2 {
		t.Errorf("want 2 tags, got %v", tags)
	}
}

func TestRemoveBackend(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	r.AddBackend(&Backend{IP: "10.0.0.1", Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.2", Up: true})
	r.RemoveBackend("10.0.0.1")

	for i := 0; i < 200; i++ {
		got := r.Lookup(fmt.Sprintf("u%d@x.com", i))
		if got == "10.0.0.1" {
			t.Fatalf("removed backend 10.0.0.1 still returned for user u%d", i)
		}
	}
}

// TestLookup_CaseInsensitiveHash proves #738: with lowercase=true two
// spellings of the same account hash (and therefore route) identically;
// with lowercase=false they diverge, reproducing the original bug.
func TestLookup_CaseInsensitiveHash(t *testing.T) {
	spellings := []string{"User@d.test", "user@d.test", "USER@D.TEST"}

	t.Run("lowercase=true routes all spellings identically", func(t *testing.T) {
		r := New(DefaultHashFormat())
		for i := 1; i <= 5; i++ {
			r.AddBackend(&Backend{IP: fmt.Sprintf("10.0.0.%d", i), Up: true})
		}
		want := r.Lookup(spellings[0])
		for _, u := range spellings[1:] {
			if got := r.Lookup(u); got != want {
				t.Errorf("Lookup(%q) = %q, want %q (same as Lookup(%q))", u, got, want, spellings[0])
			}
		}
	})

	t.Run("lowercase=false can route spellings differently", func(t *testing.T) {
		r := New(MustParseHashFormat("%u"))
		for i := 1; i <= 5; i++ {
			r.AddBackend(&Backend{IP: fmt.Sprintf("10.0.0.%d", i), Up: true})
		}
		diverged := false
		want := r.Lookup(spellings[0])
		for _, u := range spellings[1:] {
			if r.Lookup(u) != want {
				diverged = true
			}
		}
		if !diverged {
			t.Skip("chosen spellings happened to collide on this backend set — not a failure, just an uninformative run")
		}
	})
}
