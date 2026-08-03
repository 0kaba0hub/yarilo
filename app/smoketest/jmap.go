package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

func jmapHost() string {
	if *flagJMAPHost != "" {
		return *flagJMAPHost
	}
	return *flagHost
}

// checkJMAPSession proves the JMAP listener serves the session resource
// (RFC 8620 §2): GET /.well-known/jmap must return 200 and a JSON object
// carrying "capabilities". Sends Basic auth when credentials are given.
func checkJMAPSession() error {
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/.well-known/jmap"
	c := &http.Client{
		Timeout: *flagTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName:         jmapHost(),
				InsecureSkipVerify: *flagInsecure, //nolint:gosec // opt-in via -insecure
			},
		},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if *flagJMAPUser != "" {
		req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if readErr != nil {
		return fmt.Errorf("read body: %w", readErr)
	}
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}
	if len(session.Capabilities) == 0 {
		return fmt.Errorf("session resource has no capabilities: %s", body)
	}
	return nil
}
