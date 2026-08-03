package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

func jmapHost() string {
	if *flagJMAPHost != "" {
		return *flagJMAPHost
	}
	return *flagHost
}

// jmapClient dials the client-facing JMAP port, which the login proxy serves.
func jmapClient() *http.Client {
	return &http.Client{
		Timeout: *flagTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName:         jmapHost(),
				InsecureSkipVerify: *flagInsecure, //nolint:gosec // opt-in via -insecure
			},
		},
	}
}

// checkJMAPUnauthenticated proves the endpoint refuses an anonymous request.
// A 200 here would mean the backend is reachable without the login layer, which
// is the one failure mode the trust gate exists to prevent.
func checkJMAPUnauthenticated() error {
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/.well-known/jmap"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := jmapClient().Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("anonymous GET returned HTTP %d, want 401", resp.StatusCode)
	}
	return nil
}

// checkJMAPSession proves the JMAP listener serves the session resource
// (RFC 8620 §2): GET /.well-known/jmap must return 200 and a JSON object
// carrying "capabilities". Sends Basic auth when credentials are given.
func checkJMAPSession() error {
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/.well-known/jmap"
	c := jmapClient()
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

// checkJMAPBatch proves the request envelope works through the whole live
// chain: two Core/echo calls in one request, the second reading the first's
// result through a back-reference (RFC 8620 §3.7). It is the smallest probe
// that exercises dispatch, ordering and reference resolution at once.
func checkJMAPBatch() error {
	body := `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[` +
		`["Core/echo",{"ids":["m1","m2"]},"c0"],` +
		`["Core/echo",{"#ids":{"resultOf":"c0","name":"Core/echo","path":"/ids"}},"c1"]]}`
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/jmap/api/"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if *flagJMAPUser != "" {
		req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	}
	resp, err := jmapClient().Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if readErr != nil {
		return fmt.Errorf("read body: %w", readErr)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
		SessionState    string              `json:"sessionState"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(out.MethodResponses) != 2 {
		return fmt.Errorf("got %d method responses for 2 calls: %s", len(out.MethodResponses), raw)
	}
	if out.SessionState == "" {
		return fmt.Errorf("response carries no sessionState: %s", raw)
	}
	// The second response must be the echo, not an error, and must carry the
	// ids the first call produced — that is the back-reference having resolved.
	var name string
	if err := json.Unmarshal(out.MethodResponses[1][0], &name); err != nil {
		return fmt.Errorf("second response name: %w", err)
	}
	if name != "Core/echo" {
		return fmt.Errorf("second call answered with %q: %s", name, raw)
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(out.MethodResponses[1][1], &args); err != nil {
		return fmt.Errorf("second response arguments: %w", err)
	}
	if len(args.IDs) != 2 || args.IDs[0] != "m1" || args.IDs[1] != "m2" {
		return fmt.Errorf("back-reference resolved to %v, want [m1 m2]", args.IDs)
	}
	return nil
}

// checkJMAPBodyCap proves the login layer refuses an oversized body with a JMAP
// limit problem naming the bound, rather than a bare 413 or a proxied request
// the backend has to reject.
func checkJMAPBodyCap() error {
	pad := strings.Repeat("x", *flagJMAPMaxRequest+1)
	body := `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"pad":"` + pad + `"},"c0"]]}`
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/jmap/api/"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if *flagJMAPUser != "" {
		req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	}
	resp, err := jmapClient().Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		return fmt.Errorf("HTTP %d, want 413: %s", resp.StatusCode, raw)
	}
	var problem struct {
		Type  string `json:"type"`
		Limit string `json:"limit"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil {
		return fmt.Errorf("decode problem: %w", err)
	}
	if problem.Type != "urn:ietf:params:jmap:error:limit" {
		return fmt.Errorf("problem type = %q: %s", problem.Type, raw)
	}
	if problem.Limit != "maxSizeRequest" {
		return fmt.Errorf("limit member = %q, want maxSizeRequest", problem.Limit)
	}
	return nil
}
