package protocol

import "testing"

func TestExtractQuotaOverFlag(t *testing.T) {
	tests := []struct {
		name  string
		setup func() *Fields
		want  string
	}{
		{
			name:  "nil fields",
			setup: func() *Fields { return nil },
			want:  "",
		},
		{
			name:  "absent",
			setup: func() *Fields { return NewFields() },
			want:  "",
		},
		{
			name: "userdb-scoped",
			setup: func() *Fields {
				f := NewFields()
				f.Set("userdb_quota_over_flag", "TRUE")
				return f
			},
			want: "TRUE",
		},
		{
			name: "bare",
			setup: func() *Fields {
				f := NewFields()
				f.Set("quota_over_flag", "1")
				return f
			},
			want: "1",
		},
		{
			name: "userdb-scoped wins over bare",
			setup: func() *Fields {
				f := NewFields()
				f.Set("quota_over_flag", "bare")
				f.Set("userdb_quota_over_flag", "scoped")
				return f
			},
			want: "scoped",
		},
		{
			name: "empty value ignored",
			setup: func() *Fields {
				f := NewFields()
				f.Set("userdb_quota_over_flag", "")
				return f
			},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractQuotaOverFlag(tc.setup()); got != tc.want {
				t.Fatalf("extractQuotaOverFlag = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildAuthOKQuotaOverFlag verifies the flag round-trips onto the wire on
// the typed-fields path.
func TestBuildAuthOKQuotaOverFlag(t *testing.T) {
	res := &AuthResponse{Username: "u@x", QuotaOverFlag: "TRUE"}
	reply := buildAuthOK("id1", res)
	if !containsToken(reply, "quota_over_flag=TRUE") {
		t.Fatalf("reply %q missing quota_over_flag token", reply)
	}
	// Absent when empty.
	res2 := &AuthResponse{Username: "u@x"}
	if containsToken(buildAuthOK("id1", res2), "quota_over_flag=") {
		t.Fatalf("empty flag must not be emitted")
	}
}

func containsToken(reply, tok string) bool {
	for _, f := range splitTabs(reply) {
		if f == tok {
			return true
		}
	}
	return false
}

func splitTabs(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\t' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}
