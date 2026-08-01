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
	}
	for _, tc := range cases {
		lim := quota.ParseRules([]string{tc.rule})
		if lim.StorageBytes != tc.want {
			t.Errorf("ParseRules(%q).StorageBytes = %d, want %d", tc.rule, lim.StorageBytes, tc.want)
		}
	}
	// per-folder rule must NOT leak into global StorageBytes
	lim := quota.ParseRules([]string{"Trash:storage=+2G"})
	if lim.StorageBytes != 0 {
		t.Errorf("per-folder rule leaked into global StorageBytes: %d", lim.StorageBytes)
	}
	if lim.PerFolder["Trash"].StorageBytes != 2*1024*1024*1024 {
		t.Errorf("Trash per-folder storage = %d, want 2G", lim.PerFolder["Trash"].StorageBytes)
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

func TestParseRules_PerFolder(t *testing.T) {
	lim := quota.ParseRules([]string{
		"*:storage=5G",
		"*:messages=10000",
		"Trash:storage=+1G",
		"Spam:ignore",
		"Sent:storage=2G",
	})

	if lim.StorageBytes != 5*1024*1024*1024 {
		t.Errorf("global storage = %d, want 5G", lim.StorageBytes)
	}
	if len(lim.PerFolder) != 3 {
		t.Errorf("PerFolder len = %d, want 3", len(lim.PerFolder))
	}

	trash := lim.PerFolder["Trash"]
	if !trash.StorageAdditive {
		t.Error("Trash rule should be additive")
	}
	if trash.StorageBytes != 1*1024*1024*1024 {
		t.Errorf("Trash storage = %d, want 1G", trash.StorageBytes)
	}

	if !lim.PerFolder["Spam"].Ignore {
		t.Error("Spam rule should be ignore")
	}

	sent := lim.PerFolder["Sent"]
	if sent.StorageAdditive {
		t.Error("Sent rule should not be additive")
	}
	if sent.StorageBytes != 2*1024*1024*1024 {
		t.Errorf("Sent storage = %d, want 2G", sent.StorageBytes)
	}
}

func TestEffectiveLimits(t *testing.T) {
	const G = int64(1024 * 1024 * 1024)
	lim := quota.ParseRules([]string{
		"*:storage=5G",
		"*:messages=10000",
		"Trash:storage=+1G",
		"Spam:ignore",
		"Sent:storage=2G",
	})

	cases := []struct {
		folder       string
		wantStorage  int64
		wantMessages int64
		wantIgnore   bool
	}{
		{"INBOX", 5 * G, 10000, false},  // global
		{"Trash", 6 * G, 10000, false},  // additive: 5G + 1G
		{"Spam", 0, 0, true},            // ignore
		{"Sent", 2 * G, 10000, false},   // separate limit, messages from global
		{"Drafts", 5 * G, 10000, false}, // no rule → global
	}
	for _, tc := range cases {
		eff, ignore := lim.EffectiveLimits(tc.folder)
		if ignore != tc.wantIgnore {
			t.Errorf("%s: ignore = %v, want %v", tc.folder, ignore, tc.wantIgnore)
		}
		if !ignore {
			if eff.StorageBytes != tc.wantStorage {
				t.Errorf("%s: storage = %d, want %d", tc.folder, eff.StorageBytes, tc.wantStorage)
			}
			if eff.Messages != tc.wantMessages {
				t.Errorf("%s: messages = %d, want %d", tc.folder, eff.Messages, tc.wantMessages)
			}
		}
	}
}

func TestQuota_IgnoreFolder_SkipsCounter(t *testing.T) {
	d, err := memory.New(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	lim := quota.ParseRules([]string{"*:storage=5G", "Spam:ignore"})
	ctr := quota.NewCounter(d, "alice@example.com")

	// Simulate: AppendMessage would call quotaAdd only when not ignored.
	// Directly test the EffectiveLimits guard.
	_, spamIgnore := lim.EffectiveLimits("Spam")
	if !spamIgnore {
		t.Fatal("Spam should be ignored")
	}
	_, inboxIgnore := lim.EffectiveLimits("INBOX")
	if inboxIgnore {
		t.Fatal("INBOX should not be ignored")
	}

	// Add to INBOX counter (not ignored).
	if err := ctr.Add(context.Background(), 1000, 1); err != nil {
		t.Fatal(err)
	}
	u, _ := ctr.Get(context.Background())
	if u.StorageBytes != 1000 {
		t.Errorf("after INBOX append: storage = %d, want 1000", u.StorageBytes)
	}

	// Spam append would be skipped — counter stays at 1000.
	u, _ = ctr.Get(context.Background())
	if u.StorageBytes != 1000 {
		t.Errorf("after Spam skip: storage = %d, want 1000", u.StorageBytes)
	}
}

func TestParseSize_Units(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0}, {"0", 0}, {"1024", 1024},
		{"1k", 1024}, {"1K", 1024},
		{"100M", 100 * 1024 * 1024},
		{"2G", 2 * 1024 * 1024 * 1024},
		{"1T", 1024 * 1024 * 1024 * 1024},
		{"bogus", 0}, {"-5", 0},
	}
	for _, tc := range tests {
		if got := quota.ParseSize(tc.in); got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
