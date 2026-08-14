package login

import (
	"encoding/base64"
	"testing"
)

// The impersonation target is decoded and carried, not dropped. The login pod
// used to keep only authcid and password, so a master login left the pod as an
// ordinary login of the master and the auth service never saw a target (#1305).
func TestPlainDecodeKeepsTheAuthzid(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantAuthzid string
		wantUser    string
		wantErr     bool
	}{
		{
			name:        "impersonation",
			raw:         "u1@d00001.test\x00admin-master\x00masterpass",
			wantAuthzid: "u1@d00001.test",
			wantUser:    "admin-master",
		},
		{
			name:     "ordinary login has no target",
			raw:      "\x00u1@d00001.test\x00userpass",
			wantUser: "u1@d00001.test",
		},
		{
			// RFC 4616 allows authzid == authcid; it is not impersonation, and
			// the decision belongs to the auth service, so the decoder still
			// reports what the client sent.
			name:        "target equal to the authenticating identity",
			raw:         "u1@d00001.test\x00u1@d00001.test\x00userpass",
			wantAuthzid: "u1@d00001.test",
			wantUser:    "u1@d00001.test",
		},
		{
			name:    "empty authcid is refused",
			raw:     "u1@d00001.test\x00\x00pass",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authzid, user, pass, err := decodePlainCreds(base64.StdEncoding.EncodeToString([]byte(tc.raw)))
			if tc.wantErr {
				if err == nil {
					t.Fatal("a malformed payload decoded anyway")
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if authzid != tc.wantAuthzid {
				t.Errorf("authzid = %q, want %q", authzid, tc.wantAuthzid)
			}
			if user != tc.wantUser {
				t.Errorf("authcid = %q, want %q", user, tc.wantUser)
			}
			if pass == "" {
				t.Error("password lost")
			}
		})
	}
}
