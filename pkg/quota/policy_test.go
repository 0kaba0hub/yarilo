package quota

import "testing"

func TestPolicyScale(t *testing.T) {
	cases := []struct {
		name     string
		policy   Policy
		in       Limits
		wantByte int64
		wantMsg  int64
	}{
		{"zero policy is no-op", Policy{}, Limits{StorageBytes: 1000, Messages: 50}, 1000, 50},
		{"percentage 90 storage", Policy{StoragePercentage: 90}, Limits{StorageBytes: 1000}, 900, 0},
		{"percentage 90 messages", Policy{MessagePercentage: 90}, Limits{Messages: 100}, 0, 90},
		{"extra headroom", Policy{StorageExtra: 500}, Limits{StorageBytes: 1000}, 1500, 0},
		{"percentage then extra", Policy{StoragePercentage: 50, StorageExtra: 100}, Limits{StorageBytes: 1000}, 600, 0},
		{"unlimited stays unlimited", Policy{StoragePercentage: 50, StorageExtra: 100}, Limits{}, 0, 0},
		{"message extra not applied", Policy{StorageExtra: 500}, Limits{Messages: 100}, 0, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.policy.Scale(tc.in)
			if got.StorageBytes != tc.wantByte || got.Messages != tc.wantMsg {
				t.Errorf("Scale = (bytes %d, msg %d), want (%d, %d)",
					got.StorageBytes, got.Messages, tc.wantByte, tc.wantMsg)
			}
		})
	}
}

func TestIsOverWithGrace(t *testing.T) {
	lim := Limits{StorageBytes: 1000, Messages: 10}
	cases := []struct {
		name              string
		usage             Usage
		newBytes, newMsgs int64
		grace             int64
		want              bool
	}{
		{"under limit", Usage{StorageBytes: 500}, 100, 1, 0, false},
		{"exactly at limit", Usage{StorageBytes: 900}, 100, 1, 0, false},
		{"over storage no grace", Usage{StorageBytes: 950}, 100, 1, 0, true},
		{"over storage within grace", Usage{StorageBytes: 950}, 100, 1, 200, false},
		{"over storage beyond grace", Usage{StorageBytes: 950}, 300, 1, 200, true},
		{"grace does not apply to messages", Usage{Messages: 10}, 0, 1, 1000, true},
		{"message count over", Usage{Messages: 10}, 0, 1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOverWithGrace(tc.usage, lim, tc.newBytes, tc.newMsgs, tc.grace); got != tc.want {
				t.Errorf("IsOverWithGrace = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLimitsUnlimited(t *testing.T) {
	if !(Limits{}).Unlimited() {
		t.Error("zero limits should be unlimited")
	}
	if (Limits{StorageBytes: 1}).Unlimited() {
		t.Error("storage-limited should not be unlimited")
	}
	if (Limits{Messages: 1}).Unlimited() {
		t.Error("message-limited should not be unlimited")
	}
}

func TestMatchWarnings(t *testing.T) {
	limits := Limits{StorageBytes: 1000, Messages: 100}
	warns := []Warning{
		{Name: "s90", Resource: "storage", Threshold: "over", Percentage: 90},
		{Name: "m90", Resource: "message", Threshold: "over", Percentage: 90},
		{Name: "s50under", Resource: "storage", Threshold: "under", Percentage: 50},
	}
	cases := []struct {
		name          string
		before, after Usage
		want          []string
	}{
		{"cross storage 90 over", Usage{StorageBytes: 800}, Usage{StorageBytes: 950}, []string{"s90"}},
		{"already over, no cross", Usage{StorageBytes: 950}, Usage{StorageBytes: 980}, nil},
		{"cross message 90 over", Usage{Messages: 80}, Usage{Messages: 95}, []string{"m90"}},
		{"cross storage under 50", Usage{StorageBytes: 600}, Usage{StorageBytes: 400}, []string{"s50under"}},
		{"under not crossed", Usage{StorageBytes: 600}, Usage{StorageBytes: 550}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchWarnings(warns, limits, tc.before, tc.after)
			var names []string
			for _, w := range got {
				names = append(names, w.Name)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("fired %v, want %v", names, tc.want)
			}
			for i := range names {
				if names[i] != tc.want[i] {
					t.Errorf("fired %v, want %v", names, tc.want)
				}
			}
		})
	}
}

func TestMatchWarnings_UnlimitedResourceSkipped(t *testing.T) {
	// No storage limit → percentage warning has nothing to cross.
	warns := []Warning{{Name: "s90", Resource: "storage", Threshold: "over", Percentage: 90}}
	if got := MatchWarnings(warns, Limits{}, Usage{}, Usage{StorageBytes: 1 << 40}); got != nil {
		t.Errorf("unlimited resource should fire nothing, got %v", got)
	}
}
