package backend

import "testing"

func TestParseIntervalSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0}, {"0", 0}, {"30", 30}, {"45", 45},
		{"30s", 30}, {"5m", 300}, {"1h", 3600}, {"90m", 5400},
		{"-5", 0}, {"garbage", 0}, {"10x", 0},
	}
	for _, c := range cases {
		if got := parseIntervalSeconds(c.in); got != c.want {
			t.Errorf("parseIntervalSeconds(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
