// Package mtls provides helpers for building mutual-TLS configs used between
// yarilo components (auth, anvil, director, imap-login → imap, etc.).
// All inter-component connections use TLS 1.3 with client-cert verification.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ServerConfig returns a *tls.Config for mTLS servers.
// The server presents certFile/keyFile and requires clients to present a cert
// signed by the CA in caFile.
func ServerConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server cert %q: %w", certFile, err)
	}
	ca, err := loadCA(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    ca,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientConfig returns a *tls.Config for mTLS clients. The client presents
// certFile/keyFile and verifies the server against caFile, pinning serverName
// as the expected certificate name (#816). serverName is REQUIRED: internal
// services are dialled by short name / FQDN / pod IP interchangeably, so
// verifying against the dialed host is unreliable — the pinned name must be a
// SAN in the shared internal cert. An empty serverName is a misconfiguration
// (typically an empty internal_tls.server_name) and fails loudly here rather
// than as a cryptic "ServerName must be specified" on the first dial.
func ClientConfig(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, fmt.Errorf("mtls: empty ServerName — set internal_tls.server_name (or ring_tls_server_name for the director ring); mTLS peer verification needs a stable pinned name")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client cert %q: %w", certFile, err)
	}
	ca, err := loadCA(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      ca,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadCA(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: no valid CA certs in %q", caFile)
	}
	return pool, nil
}
