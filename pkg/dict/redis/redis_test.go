package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/dicttest"
)

func TestContractSuite(t *testing.T) {
	dicttest.RunSuite(t, func(t *testing.T) dict.Dict {
		mr := miniredis.RunT(t)
		d, err := New(dict.Config{Settings: map[string]any{"addr": mr.Addr()}})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		return d
	})
}

func TestPrefixIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	a, _ := New(dict.Config{Settings: map[string]any{"addr": mr.Addr(), "prefix": "ns-a:"}})
	b, _ := New(dict.Config{Settings: map[string]any{"addr": mr.Addr(), "prefix": "ns-b:"}})
	defer a.Close()
	defer b.Close()

	tx, _ := a.Begin(context.Background(), nil)
	_ = tx.Set("k", []byte("a"))
	_, _ = tx.Commit()

	_, found, _ := b.Lookup(context.Background(), nil, "k")
	if found {
		t.Error("prefix isolation broken: ns-b sees ns-a's keys")
	}
	_, foundA, _ := a.Lookup(context.Background(), nil, "k")
	if !foundA {
		t.Error("ns-a lost its own key")
	}
}

func TestTTLViaExpireSecs(t *testing.T) {
	mr := miniredis.RunT(t)
	d, _ := New(dict.Config{Settings: map[string]any{"addr": mr.Addr()}})
	defer d.Close()

	tx, _ := d.Begin(context.Background(), &dict.OpSettings{ExpireSecs: 60})
	_ = tx.Set("ttl-key", []byte("v"))
	_, _ = tx.Commit()

	// Verify Redis side sees the TTL set.
	ttl := mr.TTL("ttl-key")
	if ttl <= 0 || ttl > 60*time.Second {
		t.Errorf("expected TTL ~60s, got %v", ttl)
	}

	mr.FastForward(61 * time.Second)
	_, found, _ := d.Lookup(context.Background(), nil, "ttl-key")
	if found {
		t.Error("expired key still visible after fast-forward")
	}
}

func TestMissingAddrErrors(t *testing.T) {
	if _, err := New(dict.Config{}); err == nil {
		t.Error("missing addr should error")
	}
}

func TestRegisteredAtInit(t *testing.T) {
	for _, n := range dict.Drivers() {
		if n == "redis" {
			return
		}
	}
	t.Errorf("redis driver not registered: %v", dict.Drivers())
}
