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

// checkJMAPMailboxes proves Mailbox/get answers from the same store IMAP reads.
// It asserts the shape and the invariants that do not depend on the deployment's
// folder set: INBOX is present with the inbox role, ids are unique, and a child
// mailbox names a parent that is in the same response.
func checkJMAPMailboxes() error {
	body := `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Mailbox/get",{"accountId":"` + *flagJMAPUser + `"},"c0"]]}`
	args, err := jmapCall(body)
	if err != nil {
		return err
	}
	var res struct {
		List []struct {
			ID           string  `json:"id"`
			Name         string  `json:"name"`
			ParentID     *string `json:"parentId"`
			Role         *string `json:"role"`
			TotalEmails  uint32  `json:"totalEmails"`
			UnreadEmails uint32  `json:"unreadEmails"`
		} `json:"list"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(args, &res); err != nil {
		return fmt.Errorf("decode Mailbox/get: %w", err)
	}
	if len(res.List) == 0 {
		return fmt.Errorf("no mailboxes returned: %s", args)
	}
	if res.State == "" {
		return fmt.Errorf("Mailbox/get carries no state: %s", args)
	}
	ids := make(map[string]bool, len(res.List))
	var inbox bool
	for _, mb := range res.List {
		if mb.ID == "" {
			return fmt.Errorf("mailbox %q has no id", mb.Name)
		}
		if ids[mb.ID] {
			return fmt.Errorf("duplicate mailbox id %s", mb.ID)
		}
		ids[mb.ID] = true
		if mb.Role != nil && *mb.Role == "inbox" {
			inbox = true
			if mb.UnreadEmails > mb.TotalEmails {
				return fmt.Errorf("inbox reports %d unread of %d total", mb.UnreadEmails, mb.TotalEmails)
			}
		}
	}
	if !inbox {
		return fmt.Errorf("no mailbox carries the inbox role: %s", args)
	}
	// A parent id must name a mailbox in the same response, or the client
	// cannot build the tree.
	for _, mb := range res.List {
		if mb.ParentID != nil && !ids[*mb.ParentID] {
			return fmt.Errorf("mailbox %q names parent %s, which is not in the response", mb.Name, *mb.ParentID)
		}
	}
	return nil
}

// checkJMAPMailboxQuery proves Mailbox/query answers with the same ids
// Mailbox/get does and states plainly that it cannot calculate changes.
func checkJMAPMailboxQuery() error {
	body := `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Mailbox/query",{"accountId":"` + *flagJMAPUser + `","filter":{"role":"inbox"}},"c0"],` +
		`["Mailbox/get",{"accountId":"` + *flagJMAPUser + `",` +
		`"#ids":{"resultOf":"c0","name":"Mailbox/query","path":"/ids"}},"c1"]]}`
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(out.MethodResponses) != 2 {
		return fmt.Errorf("got %d responses for 2 calls: %s", len(out.MethodResponses), raw)
	}
	var query struct {
		IDs                 []string `json:"ids"`
		QueryState          string   `json:"queryState"`
		CanCalculateChanges bool     `json:"canCalculateChanges"`
	}
	if err := json.Unmarshal(out.MethodResponses[0][1], &query); err != nil {
		return fmt.Errorf("decode Mailbox/query: %w", err)
	}
	if len(query.IDs) != 1 {
		return fmt.Errorf("role:inbox matched %d mailboxes, want 1: %s", len(query.IDs), raw)
	}
	if query.QueryState == "" {
		return fmt.Errorf("Mailbox/query carries no queryState: %s", raw)
	}
	if query.CanCalculateChanges {
		return fmt.Errorf("canCalculateChanges is true, but Mailbox/changes is not implemented")
	}
	// The second call resolved the query's ids through a back-reference, so it
	// must have returned exactly that mailbox.
	var get struct {
		List []struct {
			ID   string  `json:"id"`
			Role *string `json:"role"`
		} `json:"list"`
	}
	if err := json.Unmarshal(out.MethodResponses[1][1], &get); err != nil {
		return fmt.Errorf("decode Mailbox/get: %w", err)
	}
	if len(get.List) != 1 || get.List[0].ID != query.IDs[0] {
		return fmt.Errorf("back-referenced Mailbox/get returned %d mailboxes: %s", len(get.List), raw)
	}
	if get.List[0].Role == nil || *get.List[0].Role != "inbox" {
		return fmt.Errorf("back-referenced mailbox is not the inbox: %s", raw)
	}
	return nil
}

// jmapCall posts one batch and returns the first response's arguments.
func jmapCall(body string) (json.RawMessage, error) {
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/jmap/api/"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if *flagJMAPUser != "" {
		req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	}
	resp, err := jmapClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(out.MethodResponses) == 0 || len(out.MethodResponses[0]) != 3 {
		return nil, fmt.Errorf("malformed response: %s", raw)
	}
	var name string
	if err := json.Unmarshal(out.MethodResponses[0][0], &name); err != nil {
		return nil, fmt.Errorf("response name: %w", err)
	}
	if name == "error" {
		return nil, fmt.Errorf("method failed: %s", out.MethodResponses[0][1])
	}
	return out.MethodResponses[0][1], nil
}

// checkJMAPEmailDiscovery is what PR6 could not probe: Email/query finds an id,
// Email/get reads it back through a back-reference, and the download endpoint
// serves the same message. One batch, so a failure names which step broke.
func checkJMAPEmailDiscovery() error {
	body := `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Email/query",{"accountId":"` + *flagJMAPUser + `","limit":1},"c0"],` +
		`["Email/get",{"accountId":"` + *flagJMAPUser + `",` +
		`"#ids":{"resultOf":"c0","name":"Email/query","path":"/ids"},` +
		`"properties":["id","blobId","subject","preview","size"]},"c1"]]}`
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(out.MethodResponses) != 2 {
		return fmt.Errorf("got %d responses for 2 calls: %s", len(out.MethodResponses), raw)
	}
	var query struct {
		IDs                 []string `json:"ids"`
		QueryState          string   `json:"queryState"`
		CanCalculateChanges bool     `json:"canCalculateChanges"`
		Limit               *uint    `json:"limit"`
	}
	if err := json.Unmarshal(out.MethodResponses[0][1], &query); err != nil {
		return fmt.Errorf("decode Email/query: %w", err)
	}
	if len(query.IDs) == 0 {
		return fmt.Errorf("Email/query found no messages — the mailbox must not be empty for this check: %s", raw)
	}
	if query.QueryState == "" {
		return fmt.Errorf("Email/query carries no queryState")
	}
	if query.CanCalculateChanges {
		return fmt.Errorf("canCalculateChanges is true, but Email/changes is not implemented")
	}
	// The server applied a limit, so it must say which one.
	if query.Limit == nil {
		return fmt.Errorf("Email/query applied a limit without reporting it: %s", raw)
	}
	var get struct {
		List []struct {
			ID     string `json:"id"`
			BlobID string `json:"blobId"`
		} `json:"list"`
	}
	if err := json.Unmarshal(out.MethodResponses[1][1], &get); err != nil {
		return fmt.Errorf("decode Email/get: %w", err)
	}
	if len(get.List) != 1 || get.List[0].ID != query.IDs[0] {
		return fmt.Errorf("back-referenced Email/get returned %d emails: %s", len(get.List), raw)
	}

	// The blob the same batch named must download, and be non-empty.
	dl := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) +
		"/jmap/download/" + *flagJMAPUser + "/" + get.List[0].BlobID + "/message.eml"
	dreq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, dl, nil)
	if err != nil {
		return err
	}
	if *flagJMAPUser != "" {
		dreq.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	}
	dresp, err := jmapClient().Do(dreq)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		return fmt.Errorf("download HTTP %d", dresp.StatusCode)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(dresp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("download read: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("download returned an empty body")
	}
	if ct := dresp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		return fmt.Errorf("download Content-Type = %q, want application/octet-stream", ct)
	}
	return nil
}

// checkJMAPDownloadIsolation proves a blob is refused for another account
// rather than served — ownership is the precondition the endpoint rests on.
func checkJMAPDownloadIsolation() error {
	dl := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) +
		"/jmap/download/somebody-else@example.invalid/deadbeef/message.eml"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, dl, nil)
	if err != nil {
		return err
	}
	if *flagJMAPUser != "" {
		req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	}
	resp, err := jmapClient().Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("HTTP %d, want 404 for another account's blob", resp.StatusCode)
	}
	return nil
}
