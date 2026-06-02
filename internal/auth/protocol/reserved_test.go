package protocol

import (
	"errors"
	"testing"
)

func TestValidateUint32(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"0", "0", false},
		{"1001", "1001", false},
		{"4294967295", "4294967295", false},
		{"  1001  ", "1001", false},
		{"-1", "", true},
		{"4294967296", "", true},
		{"abc", "", true},
		{"", "", true},
		{"1.5", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateUint32(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateInt(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"0", "0", false},
		{"-30", "-30", false},
		{"143", "143", false},
		{"  587  ", "587", false},
		{"abc", "", true},
		{"", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateInt(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateBool(t *testing.T) {
	truthy := []string{"yes", "YES", "true", "True", "on", "1", "t", "y", " yes "}
	falsy := []string{"no", "NO", "false", "off", "0", "f", "n", "  off"}
	bad := []string{"maybe", "2", "", "tt", "yess"}

	for _, in := range truthy {
		got, err := validateBool(in)
		if err != nil || got != "yes" {
			t.Errorf("validateBool(%q) = %q,%v; want yes,nil", in, got, err)
		}
	}
	for _, in := range falsy {
		got, err := validateBool(in)
		if err != nil || got != "no" {
			t.Errorf("validateBool(%q) = %q,%v; want no,nil", in, got, err)
		}
	}
	for _, in := range bad {
		if _, err := validateBool(in); err == nil {
			t.Errorf("validateBool(%q) should error", in)
		}
	}
}

func TestValidateCIDRList(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"single ipv4", "10.0.0.0/8", "10.0.0.0/8", false},
		{"single ipv6", "fe80::/10", "fe80::/10", false},
		{"bare ipv4 promoted to /32", "192.168.1.5", "192.168.1.5/32", false},
		{"bare ipv6 promoted to /128", "2001:db8::1", "2001:db8::1/128", false},
		{"mixed list", "10.0.0.0/8, 192.168.0.0/16, 172.16.0.0/12",
			"10.0.0.0/8,192.168.0.0/16,172.16.0.0/12", false},
		{"bare + cidr mix", "10.0.0.5, 192.168.0.0/16",
			"10.0.0.5/32,192.168.0.0/16", false},
		{"invalid garbage", "not-a-cidr", "", true},
		{"empty list", "", "", true},
		{"trailing comma tolerated", "10.0.0.0/8,", "10.0.0.0/8", false},
		{"non-canonical CIDR gets canonicalised", "10.0.0.5/8", "10.0.0.0/8", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateCIDRList(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateNonEmptyCSV(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"staff", "staff", false},
		{"staff,mail,wheel", "staff,mail,wheel", false},
		{"staff, mail , wheel ", "staff,mail,wheel", false},
		{"", "", true},
		{" , , ", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateNonEmptyCSV(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateSSLEnum(t *testing.T) {
	for _, in := range []string{"yes", "any", "required", "YES", "Required"} {
		got, err := validateSSLEnum(in)
		if err != nil {
			t.Errorf("validateSSLEnum(%q) errored: %v", in, err)
		}
		want := "yes"
		switch in {
		case "any":
			want = "any"
		case "required", "Required":
			want = "required"
		}
		if got != want {
			t.Errorf("validateSSLEnum(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"maybe", "no", "true", ""} {
		if _, err := validateSSLEnum(in); err == nil {
			t.Errorf("validateSSLEnum(%q) should error", in)
		}
	}
}

func TestFields_SetValidated_KnownKeyCanonicalises(t *testing.T) {
	f := NewFields()
	if err := f.SetValidated("nologin", "TRUE"); err != nil {
		t.Fatalf("SetValidated: %v", err)
	}
	if v, _ := f.Get("nologin"); v != "yes" {
		t.Errorf("nologin = %q, want canonical yes", v)
	}
}

func TestFields_SetValidated_UnknownKeyPassesThrough(t *testing.T) {
	f := NewFields()
	if err := f.SetValidated("custom_field", "anything goes\x00here"); err != nil {
		t.Fatalf("SetValidated: %v", err)
	}
	if v, _ := f.Get("custom_field"); v != "anything goes\x00here" {
		t.Errorf("custom_field = %q (verbatim expected)", v)
	}
}

func TestFields_SetValidated_UserdbPrefixUnwraps(t *testing.T) {
	f := NewFields()
	if err := f.SetValidated("userdb_uid", "1001"); err != nil {
		t.Fatalf("SetValidated userdb_uid: %v", err)
	}
	if v, _ := f.Get("userdb_uid"); v != "1001" {
		t.Errorf("userdb_uid = %q", v)
	}

	if err := f.SetValidated("userdb_uid", "not-a-number"); err == nil {
		t.Error("expected error on userdb_uid with non-numeric value")
	}
	// Failed validation MUST NOT mutate.
	if v, _ := f.Get("userdb_uid"); v != "1001" {
		t.Errorf("failed SetValidated mutated bag: userdb_uid = %q", v)
	}
}

func TestFields_SetValidated_ForwardPrefixPassesThrough(t *testing.T) {
	// forward_* fields are by design opaque to the chain — even
	// when they would otherwise look like a reserved key, the
	// prefix preserves the raw value.
	f := NewFields()
	if err := f.SetValidated("forward_uid", "not-a-uint32-but-fine"); err != nil {
		t.Fatalf("SetValidated forward_uid: %v", err)
	}
	if v, _ := f.Get("forward_uid"); v != "not-a-uint32-but-fine" {
		t.Errorf("forward_uid = %q (should pass through)", v)
	}
}

func TestFields_SetValidated_ErrorDoesNotMutate(t *testing.T) {
	f := NewFields()
	f.Set("uid", "100")
	err := f.SetValidated("uid", "garbage")
	if err == nil {
		t.Fatal("want error")
	}
	if v, _ := f.Get("uid"); v != "100" {
		t.Errorf("uid mutated after failed SetValidated: %q", v)
	}
}

func TestFields_SetValidated_ErrorWrapsKey(t *testing.T) {
	f := NewFields()
	err := f.SetValidated("allow_nets", "not-a-cidr")
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, err) || !contains(err.Error(), `field "allow_nets"`) {
		t.Errorf("error %q should name the field", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
