package language

import "testing"

func TestValidateTokenizerConfig(t *testing.T) {
	tests := []struct {
		name           string
		algorithm      string
		wb5a           bool
		explicitPrefix bool
		wantErr        bool
	}{
		{"empty algorithm defaults to simple", "", false, false, false},
		{"simple algorithm ok", "simple", false, false, false},
		{"tr29 not yet implemented", "tr29", false, false, true},
		{"unknown algorithm rejected", "bogus", false, false, true},
		{"wb5a not yet implemented", "simple", true, false, true},
		{"explicit_prefix not yet implemented", "simple", false, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTokenizerConfig(tc.algorithm, tc.wb5a, tc.explicitPrefix)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateTokenizerConfig(%q, %v, %v) err = %v, wantErr %v",
					tc.algorithm, tc.wb5a, tc.explicitPrefix, err, tc.wantErr)
			}
		})
	}
}
