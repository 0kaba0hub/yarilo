package config

import "testing"

func withCert() SSLConfig { return SSLConfig{TLSCert: "/tls/tls.crt", TLSKey: "/tls/tls.key"} }
func noCert() SSLConfig   { return SSLConfig{} }
func svcMode(m string) *ServiceConfig {
	return &ServiceConfig{Enabled: true, Port: 993, SSLMode: m}
}

// A listener declaring implicit TLS without a certificate would bind and speak
// its protocol in the clear on a port that is TLS by definition, so it is an
// error rather than a downgrade.
func TestCheckListenerTLS(t *testing.T) {
	tests := []struct {
		name     string
		svc      *ServiceConfig
		ssl      SSLConfig
		wantErr  bool
		wantWarn bool
	}{
		{name: "ssl with a certificate", svc: svcMode("ssl"), ssl: withCert()},
		{name: "ssl without a certificate", svc: svcMode("ssl"), ssl: noCert(), wantErr: true},
		{name: "starttls without a certificate warns", svc: svcMode("starttls"), ssl: noCert(), wantWarn: true},
		{name: "starttls with a certificate", svc: svcMode("starttls"), ssl: withCert()},
		{name: "plaintext listener is unaffected", svc: svcMode("no"), ssl: noCert()},
		{name: "empty mode is unaffected", svc: svcMode(""), ssl: noCert()},
		{name: "disabled listener is not checked", svc: &ServiceConfig{Enabled: false, SSLMode: "ssl"}, ssl: noCert()},
		{name: "absent listener is not checked", svc: nil, ssl: noCert()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, err := CheckListenerTLS("services.test", tt.svc, tt.ssl)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if (warn != "") != tt.wantWarn {
				t.Errorf("warn = %q, wantWarn %v", warn, tt.wantWarn)
			}
		})
	}
}

// A per-service ssl block is what that listener actually presents, so it is
// what gets checked.
func TestCheckListenerTLSPrefersTheServiceBlock(t *testing.T) {
	svc := svcMode("ssl")
	own := withCert()
	svc.SSL = &own
	if _, err := CheckListenerTLS("services.test", svc, noCert()); err != nil {
		t.Errorf("a service-level certificate was ignored: %v", err)
	}

	svc2 := svcMode("ssl")
	empty := noCert()
	svc2.SSL = &empty
	if _, err := CheckListenerTLS("services.test", svc2, withCert()); err == nil {
		t.Error("an empty service block should not inherit general.ssl")
	}
}
