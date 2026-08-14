package config

import "testing"

// A template nothing can expand must stop startup. Passing it through is how a
// directory named "%{user | sha1 % 256 | hex(2)}" gets created and used.
func TestValidatePathTemplatesRefusesWhatCannotExpand(t *testing.T) {
	tests := []struct {
		name    string
		storage StorageConfig
		wantErr bool
	}{
		{"reference 2.4 form", StorageConfig{MailVolatilePath: "/tmp/v/%{user | sha1 % 256 | hex(2)}/%{user}"}, false},
		{"legacy form", StorageConfig{MailVolatilePath: "/tmp/v/%2.256Nu/%u"}, false},
		{"home template", StorageConfig{MailHome: "%d/%u"}, false},
		{"no templates at all", StorageConfig{MailPath: "/var/mail"}, false},
		{"unknown filter", StorageConfig{MailVolatilePath: "/tmp/v/%{user | nope}"}, true},
		{"unknown variable", StorageConfig{MailIndexPath: "/idx/%{nope}"}, true},
		{"unknown short variable", StorageConfig{MailControlPath: "/ctl/%z"}, true},
		{"unclosed expression", StorageConfig{MailAltPath: "/alt/%{user"}, true},
		{"hash left as bytes", StorageConfig{MailAltPath: "/cold/%{user | sha1}"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePathTemplates(&tc.storage)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil && !contains(err.Error(), "storage.") {
				t.Errorf("error does not name the key that is wrong: %v", err)
			}
		})
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
