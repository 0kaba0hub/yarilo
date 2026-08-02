package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/yarilomail/yarilo/pkg/mtls"
)

var (
	httpClientOnce sync.Once
	httpClient     *http.Client
	httpClientErr  error
)

// adminHTTPClient returns the HTTP client for the director/backend-api hops.
// With internal mTLS configured (CA, ServerName, or client cert set, #954) it
// presents the client cert, trusts the internal CA, and verifies the server
// against the pinned ServerName (the URL host is an IP/localhost that never
// matches). Otherwise http.DefaultClient. Built once so the TLS session cache
// is shared.
func adminHTTPClient() (*http.Client, error) {
	httpClientOnce.Do(func() {
		httpClient, httpClientErr = newAdminClient(tlsCert, tlsKey, tlsCA, tlsServerName)
	})
	return httpClient, httpClientErr
}

// newAdminClient builds the admin-plane HTTP client. With no internal mTLS
// inputs set it returns http.DefaultClient (plain HTTP); otherwise it requires
// cert+key+ca+serverName and dials over mTLS. Pure (no globals) for testability.
func newAdminClient(cert, key, ca, serverName string) (*http.Client, error) {
	if ca == "" && cert == "" && serverName == "" {
		return http.DefaultClient, nil
	}
	cfg, err := mtls.ClientConfig(cert, key, ca, serverName, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("internal mTLS client config (set --tls-cert/-key/-ca/-server-name or YARILO_ADMIN_TLS_*): %w", err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}

func apiGet(path string) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodGet, path, nil)
}
func apiPost(path string, body any) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodPost, path, body)
}
func apiPatch(path string, body any) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodPatch, path, body)
}
func apiDelete(path string) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodDelete, path, nil)
}

// backendAPIGet / backendAPIPost talk to the yarilo-backend-api endpoint
// (dict/acl/quota/folder/user/mailbox); director-plane ops stay on apiGet/apiPost.
//
// Per-user routing (#792): backend-api listens on the pod IP, so a per-user op
// must reach the pod the user is pinned to. When routeByUser is on, these
// chokepoints extract the user (query user= for GET, body "user" for POST) and
// resolve the pod via a director LOOKUP; the admin never picks a pod itself
// (that would race a login and split the per-user FTS writer). Requests with no
// user (dict, iterate) keep the fixed backendAPIURL.
func backendAPIGet(path string) ([]byte, error) {
	base, err := backendBaseForUser(resolveBackendUser(path, nil))
	if err != nil {
		return nil, err
	}
	return doRequest(base, backendAPIToken, http.MethodGet, path, nil)
}

func backendAPIPost(path string, body any) ([]byte, error) {
	base, err := backendBaseForUser(resolveBackendUser(path, body))
	if err != nil {
		return nil, err
	}
	return doRequest(base, backendAPIToken, http.MethodPost, path, body)
}

// backendAPIStream sends a POST and returns the raw response body
// as an io.ReadCloser so streaming endpoints (NDJSON iterate) can
// be consumed line-by-line. Caller MUST Close the body.
func backendAPIStream(path string, body any) (io.ReadCloser, error) {
	base, err := backendBaseForUser(resolveBackendUser(path, body))
	if err != nil {
		return nil, err
	}
	resp, err := doRawRequest(base, backendAPIToken, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		return nil, apiError(resp, data)
	}
	return resp.Body, nil
}

// resolveBackendUser extracts the target user from a backend-api request: the
// `user` query param (GET) or the top-level "user" key of a map body (POST).
// Returns "" for global ops (dict uses typed struct bodies; iterate/count carry
// no user) — those keep the fixed backendAPIURL.
func resolveBackendUser(path string, body any) string {
	if u, ok := body.(map[string]any); ok {
		if s, ok := u["user"].(string); ok && s != "" {
			return s
		}
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		if q, err := url.ParseQuery(path[i+1:]); err == nil {
			return q.Get("user")
		}
	}
	return ""
}

// backendBaseForUser returns the backend-api base URL for a request. With
// routing off or no user it uses the fixed backendAPIURL (standalone/global
// ops); otherwise it asks the director which pod owns the user and targets that
// pod's backend-api port. A director that is down or has no backend errors
// cleanly, never falling back to a random pod (which would write the wrong
// pod's per-user state).
func backendBaseForUser(user string) (string, error) {
	if !routeByUser || user == "" {
		return backendAPIURL, nil
	}
	data, err := apiGet("/api/director/map?user=" + url.QueryEscape(user))
	if err != nil {
		return "", fmt.Errorf("resolve backend for user %q via director (%s): %w", user, apiURL, err)
	}
	var m struct {
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.Backend == "" {
		return "", fmt.Errorf("director returned no backend for user %q", user)
	}
	// Match the scheme backend-api serves: https under internal mTLS (#954), else
	// http. The host is a pod IP, so verification relies on the pinned
	// --tls-server-name, not the host.
	return fmt.Sprintf("%s://%s:%d", adminScheme(), m.Backend, backendAPIPort), nil
}

// adminScheme is "https" when internal mTLS is configured for the admin hops,
// else "http". Used for the per-user pod URL the director resolves (#954).
func adminScheme() string {
	if tlsCA != "" || tlsServerName != "" || tlsCert != "" {
		return "https"
	}
	return "http"
}

func doRequest(baseURL, token, method, path string, body any) ([]byte, error) {
	resp, err := doRawRequest(baseURL, token, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, apiError(resp, data)
	}
	return data, nil
}

// apiError turns a >=400 response into a clean operator-facing error. A JSON
// {"error": "..."} envelope surfaces only its message; non-JSON or empty bodies
// fall back to the HTTP status line.
func apiError(resp *http.Response, body []byte) error {
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != "" {
		return fmt.Errorf("%s", env.Error)
	}
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return fmt.Errorf("HTTP %s: %s", resp.Status, msg)
	}
	return fmt.Errorf("HTTP %s", resp.Status)
}

func doRawRequest(baseURL, token, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, strings.TrimRight(baseURL, "/")+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client, err := adminHTTPClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func printJSON(data []byte, err error) error {
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if jsonErr := json.Indent(&buf, data, "", "  "); jsonErr != nil {
		fmt.Println(string(data))
		return nil
	}
	fmt.Println(buf.String())
	return nil
}

// printOutput dispatches to a human renderer when -O human is set,
// falling back to JSON for commands that have not yet implemented one.
// humanFn receives the raw response bytes; if nil, JSON is used regardless.
func printOutput(data []byte, err error, humanFn func([]byte) error) error {
	if err != nil {
		return err
	}
	if outputFormat == "human" && humanFn != nil {
		return humanFn(data)
	}
	return printJSON(data, nil)
}
