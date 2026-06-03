package quota_test

import (
	"context"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/dict/memory"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

func TestParseRules_StorageUnits(t *testing.T) {
	cases := []struct {
		rule string
		want int64
	}{
		{"*:storage=5G", 5 * 1024 * 1024 * 1024},
		{"*:storage=500M", 500 * 1024 * 1024},
		{"*:storage=1T", 1024 * 1024 * 1024 * 1024},
		{"*:storage=1024K", 1024 * 1024},
		{"*:storage=0", 0},
		{"*:storage=1234567", 1234567},
		{"Trash:storage=+2G", 2 * 1024 * 1024 * 1024},
	}
	for _, tc := range cases {
		lim := quota.ParseRules([]string{tc.rule})
		if lim.StorageBytes != tc.want {
			t.Errorf("ParseRules(%q).StorageBytes = %d, want %d", tc.rule, lim.StorageBytes, tc.want)
		}
	}
}

func TestParseRules_Messages(t *testing.T) {
	lim := quota.ParseRules([]string{"*:messages=100000"})
	if lim.Messages != 100000 {
		t.Errorf("Messages = %d, want 100000", lim.Messages)
	}
}

func TestParseRules_MultipleRules(t *testing.T) {
	lim := quota.ParseRules([]string{"*:storage=5G", "*:messages=50000"})
	if lim.StorageBytes != 5*1024*1024*1024 {
		t.Errorf("StorageBytes = %d", lim.StorageBytes)
	}
	if lim.Messages != 50000 {
		t.Errorf("Messages = %d", lim.Messages)
	}
}

func TestParseRules_Empty(t *testing.T) {
	lim := quota.ParseRules(nil)
	if lim.StorageBytes != 0 || lim.Messages != 0 {
		t.Errorf("empty rules: got %+v", lim)
	}
}

func TestStorageBytesToKiB(t *testing.T) {
	cases := []struct{ bytes, wantKiB int64 }{
		{0, 0},
		{1024, 1},
		{1025, 2}, // round up
		{2048, 2},
		{5 * 1024 * 1024 * 1024, 5 * 1024 * 1024}, // 5 GiB → 5242880 KiB
	}
	for _, tc := range cases {
		got := quota.StorageBytesToKiB(tc.bytes)
		if int64(got) != tc.wantKiB {
			t.Errorf("StorageBytesToKiB(%d) = %d, want %d", tc.bytes, got, tc.wantKiB)
		}
	}
}

func TestCounter_AddAndGet(t *testing.T) {
	d, _ := memory.New(dict.Config{})
	ctr := quota.NewCounter(d, "alice@example.com")
	ctx := context.Background()

	u, err := ctr.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.StorageBytes != 0 || u.Messages != 0 {
		t.Errorf("fresh counter non-zero: %+v", u)
	}

	if err := ctr.Add(ctx, 1024, 1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	u, err = ctr.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.StorageBytes != 1024 {
		t.Errorf("StorageBytes = %d, want 1024", u.StorageBytes)
	}
	if u.Messages != 1 {
		t.Errorf("Messages = %d, want 1", u.Messages)
	}

	// Decrement on expunge.
	if err := ctr.Add(ctx, -512, -1); err != nil {
		t.Fatalf("Add decrement: %v", err)
	}
	u, _ = ctr.Get(ctx)
	if u.StorageBytes != 512 {
		t.Errorf("after decrement StorageBytes = %d, want 512", u.StorageBytes)
	}
	if u.Messages != 0 {
		t.Errorf("after decrement Messages = %d, want 0", u.Messages)
	}
}

func TestCounter_Set(t *testing.T) {
	d, _ := memory.New(dict.Config{})
	ctr := quota.NewCounter(d, "bob@example.com")
	ctx := context.Background()

	want := quota.Usage{StorageBytes: 999, Messages: 42}
	if err := ctr.Set(ctx, want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := ctr.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("after Set: got %+v, want %+v", got, want)
	}
}

func TestIsOver(t *testing.T) {
	lim := quota.Limits{StorageBytes: 1024, Messages: 10}
	cases := []struct {
		u        quota.Usage
		newBytes int64
		newMsgs  int64
		over     bool
	}{
		{quota.Usage{StorageBytes: 0, Messages: 0}, 512, 1, false},
		{quota.Usage{StorageBytes: 512, Messages: 5}, 513, 1, true},  // exceeds bytes
		{quota.Usage{StorageBytes: 0, Messages: 9}, 1, 2, true},      // exceeds messages
		{quota.Usage{StorageBytes: 1024, Messages: 10}, 0, 0, false}, // exactly at limit is OK
		{quota.Usage{StorageBytes: 1024, Messages: 10}, 1, 0, true},  // one byte over
	}
	for i, tc := range cases {
		got := quota.IsOver(tc.u, lim, tc.newBytes, tc.newMsgs)
		if got != tc.over {
			t.Errorf("[%d] IsOver = %v, want %v", i, got, tc.over)
		}
	}
}

func TestIsOver_Unlimited(t *testing.T) {
	lim := quota.Limits{} // both zero = unlimited
	if quota.IsOver(quota.Usage{StorageBytes: 1 << 40}, lim, 1<<40, 1<<30) {
		t.Error("unlimited limits should never be over")
	}
}
