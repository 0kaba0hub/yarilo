package quota

import "testing"

func TestWildcardMatchIcase(t *testing.T) {
	cases := []struct {
		s, pattern string
		want       bool
	}{
		{"TRUE", "TRUE", true},
		{"true", "TRUE", true}, // case-insensitive
		{"TRUE", "*", true},
		{"", "*", true},
		{"anything", "*", true},
		{"over_quota", "over*", true},
		{"over_quota", "*quota", true},
		{"over_quota", "over?quota", true},
		{"FALSE", "TRUE", false},
		{"over_quota", "under*", false},
		{"ab", "a?c", false},
		{"abc", "a?c", true},
	}
	for _, tc := range cases {
		if got := WildcardMatchIcase(tc.s, tc.pattern); got != tc.want {
			t.Errorf("WildcardMatchIcase(%q, %q) = %v, want %v", tc.s, tc.pattern, got, tc.want)
		}
	}
}

func TestIsOverAny(t *testing.T) {
	cases := []struct {
		name   string
		u      Usage
		limits Limits
		want   bool
	}{
		{"under both", Usage{StorageBytes: 500, Messages: 5}, Limits{StorageBytes: 1000, Messages: 10}, false},
		{"storage at limit", Usage{StorageBytes: 1000}, Limits{StorageBytes: 1000}, true},
		{"storage over", Usage{StorageBytes: 1200}, Limits{StorageBytes: 1000}, true},
		{"messages at limit", Usage{Messages: 10}, Limits{Messages: 10}, true},
		{"unlimited never over", Usage{StorageBytes: 1 << 40}, Limits{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOverAny(tc.u, tc.limits); got != tc.want {
				t.Errorf("IsOverAny = %v, want %v", got, tc.want)
			}
		})
	}
}
