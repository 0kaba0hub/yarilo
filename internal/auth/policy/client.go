// Package policy implements a wforce-compatible HTTP policy client. The
// auth server POSTs every login attempt to an external policy server;
// the response decides whether to continue, tarpit, or reject.
//
// The wire shape (JSON payload keys, hash algorithm, status semantics,
// command query-string) matches the wforce protocol so operators can
// reuse the same wforce instance across deployments.
package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Mode controls which calls the auth path makes to the policy server.
// The three flags are orthogonal; any combination is legal.
type Mode struct {
	// CheckBefore: POST ?command=allow BEFORE the passdb runs. status<0
	// rejects the login; status>0 tarpits that many seconds before the
	// chain runs.
	CheckBefore bool

	// CheckAfter: POST ?command=allow after the passdb result is known
	// but before the reply hits the wire. Same status semantics; here
	// the server can downgrade a successful auth to fail
	// (account-takeover detection).
	CheckAfter bool

	// ReportAfter: POST ?command=report once the decision is final.
	// Fire-and-forget. Payload carries `success` and `policy_reject`.
	ReportAfter bool
}

// Config tunes the policy client. URL="" disables policy entirely
// (every method becomes a no-op returning continue).
type Config struct {
	// URL is the policy server endpoint. Empty disables.
	URL string

	// APIHeader is added to every request. Two formats:
	//   - "Key: value"          → custom header
	//   - "secret"  (no colon)  → X-API-Key: secret
	APIHeader string

	// HashMech is the digest used to obfuscate username+password
	// in the `pwhash` field. Allowed: "sha256", "sha512". Default
	// "sha256". Must match wforce's hash setting.
	HashMech string

	// HashNonce is the per-deployment salt mixed into the hash. Required
	// when URL is set; an empty nonce is rejected at construction so two
	// deployments don't share the same pwhash space.
	HashNonce string

	// HashTruncateBits is how many MSB bits of the digest survive into
	// pwhash. Default 12 (4096 buckets — enough for rate-limit patterns,
	// useless for password recovery). 0 = no truncation (full hash).
	HashTruncateBits uint

	// Timeout caps the HTTP round-trip. RejectOnFail decides what happens
	// when it fires.
	Timeout time.Duration

	// RejectOnFail flips the default-allow stance: when true, an
	// unreachable / non-2xx / malformed-JSON policy server rejects the
	// login. Default false (fail-open).
	RejectOnFail bool

	// LogOnly: still POST and log decisions, but Check methods return
	// continue regardless of the server's response. For shadow-mode
	// rollout before enforcing.
	LogOnly bool

	// HTTPClient is the underlying transport. nil → http.DefaultClient
	// with a Timeout wrapper.
	HTTPClient *http.Client
}

// Decision is the parsed policy-server result. Continue=true means
// proceed (status==0). Reject=true means refused (status<0).
// TarpitSecs>0 (with Continue=true) means sleep that many seconds, then
// proceed.
type Decision struct {
	Continue   bool
	Reject     bool
	TarpitSecs int
	Message    string
}

// Request is the structured input to a policy call. The client hashes
// (username, password) and POSTs the JSON template wforce expects.
type Request struct {
	Username  string // resolved/requested username before chain
	Password  string // plain — used only to compute pwhash, never stored
	RemoteIP  string // client IP as a string ("" allowed)
	Service   string // "imap" / "pop3" / "smtp" / "lmtp"
	DeviceID  string // free-form client tag (empty allowed)
	SessionID string // unique per auth attempt (empty allowed)
	TLS       bool   // whether the client connection was TLS-secured
	// FailType is the post-auth classification. Only used by
	// CheckAfter and ReportAfter. Empty for CheckBefore.
	//   "" / "policy" / "internal" / "credentials" / "expired"
	//   / "disabled" / "account"
	FailType string
}

// Client is the policy-server HTTP client. Safe for concurrent use.
type Client struct {
	cfg Config
	hc  *http.Client
}

var (
	// ErrPolicyReject is returned by Check* on server status<0 (explicit
	// refuse). Caller surfaces it as an opaque auth-fail.
	ErrPolicyReject = errors.New("policy: rejected")
	// ErrPolicyFail is returned by Check* when the server was
	// unreachable / malformed and RejectOnFail is true. Caller surfaces
	// it as temp_fail.
	ErrPolicyFail = errors.New("policy: server unavailable")
)

// New builds a Client from cfg. Returns (nil, nil) when cfg.URL is empty
// (policy disabled). Invalid config (URL set but nonce empty, unknown
// hash mech) returns (nil, error).
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, nil
	}
	if cfg.HashNonce == "" {
		return nil, errors.New("policy: HashNonce must be set when URL is non-empty")
	}
	switch strings.ToLower(cfg.HashMech) {
	case "", "sha256", "sha512":
		// ok
	default:
		return nil, fmt.Errorf("policy: unsupported hash mech %q (sha256, sha512)", cfg.HashMech)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.HashTruncateBits == 0 {
		cfg.HashTruncateBits = 12
	}
	if cfg.HashMech == "" {
		cfg.HashMech = "sha256"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{cfg: cfg, hc: hc}, nil
}

// CheckBefore POSTs ?command=allow with `success` / `policy_reject`
// omitted, returning the parsed decision.
func (c *Client) CheckBefore(ctx context.Context, req Request) (Decision, error) {
	if c == nil {
		return Decision{Continue: true}, nil
	}
	return c.do(ctx, "allow", req, false)
}

// CheckAfter is CheckBefore called once the passdb result is known. The
// server may downgrade success to fail (account-takeover detection).
func (c *Client) CheckAfter(ctx context.Context, req Request, success bool, policyReject bool) (Decision, error) {
	if c == nil {
		return Decision{Continue: true}, nil
	}
	return c.doWithStatus(ctx, "allow", req, true, success, policyReject)
}

// ReportAfter fires the post-decision telemetry call. The body is
// discarded; errors are logged, not surfaced.
func (c *Client) ReportAfter(ctx context.Context, req Request, success bool, policyReject bool) {
	if c == nil {
		return
	}
	_, err := c.doWithStatus(ctx, "report", req, true, success, policyReject)
	if err != nil {
		slog.Debug("auth/policy: report failed",
			"service", req.Service, "err", err)
	}
}

// do is the simple variant: no `success` / `policy_reject` fields.
func (c *Client) do(ctx context.Context, command string, req Request, _ bool) (Decision, error) {
	return c.doWithStatus(ctx, command, req, false, false, false)
}

// doWithStatus is the actual HTTP round-trip, shared by both Check
// variants and ReportAfter. The booleans gate emission of the
// post-decision fields.
func (c *Client) doWithStatus(ctx context.Context, command string, req Request,
	includeStatus, success, policyReject bool) (Decision, error) {

	body, err := c.buildPayload(req, includeStatus, success, policyReject)
	if err != nil {
		return c.failoverDecision(), fmt.Errorf("build payload: %w", err)
	}

	url := joinCommand(c.cfg.URL, command)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return c.failoverDecision(), fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIHeader != "" {
		k, v := splitAPIHeader(c.cfg.APIHeader)
		httpReq.Header.Set(k, v)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return c.failoverDecision(), fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Read + discard so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return c.failoverDecision(), fmt.Errorf("policy server HTTP %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return c.failoverDecision(), fmt.Errorf("read response: %w", err)
	}

	if command == "report" {
		// Report mode has no meaningful body (typically 200 OK, empty).
		return Decision{Continue: true}, nil
	}

	var parsed struct {
		Status int    `json:"status"`
		Msg    string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return c.failoverDecision(), fmt.Errorf("parse response: %w", err)
	}

	d := Decision{Message: parsed.Msg}
	switch {
	case parsed.Status == 0:
		d.Continue = true
	case parsed.Status < 0:
		d.Reject = true
	case parsed.Status > 0:
		d.Continue = true
		d.TarpitSecs = parsed.Status
	}

	if c.cfg.LogOnly && !d.Continue {
		slog.Info("auth/policy: log-only mode ignoring reject",
			"service", req.Service,
			"user", req.Username,
			"remote", req.RemoteIP,
			"msg", d.Message,
		)
		return Decision{Continue: true}, nil
	}
	return d, nil
}

// failoverDecision is returned when the round-trip fails or the response
// is malformed. RejectOnFail flips fail-open to fail-closed.
func (c *Client) failoverDecision() Decision {
	if c.cfg.RejectOnFail {
		return Decision{Reject: true, Message: "policy server unavailable"}
	}
	return Decision{Continue: true}
}

// buildPayload assembles the JSON body. Keys are alphabetic to match
// the wforce wire format.
func (c *Client) buildPayload(req Request, includeStatus, success, policyReject bool) ([]byte, error) {
	type field struct {
		key   string
		value interface{}
	}
	fields := []field{
		{"device_id", req.DeviceID},
		{"fail_type", req.FailType},
		{"login", req.Username},
		{"protocol", req.Service},
		{"pwhash", c.hashPassword(req.Username, req.Password)},
		{"remote", req.RemoteIP},
		{"session_id", req.SessionID},
		{"tls", req.TLS},
	}
	if includeStatus {
		fields = append(fields,
			field{"policy_reject", policyReject},
			field{"success", success},
		)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].key < fields[j].key })

	m := make(map[string]interface{}, len(fields))
	for _, f := range fields {
		m[f.key] = f.value
	}
	return json.Marshal(m)
}

// hashPassword returns the hex-encoded, truncated digest of
// `nonce + username + \0 + password`, matching the sequence wforce
// understands.
func (c *Client) hashPassword(username, password string) string {
	var h hash.Hash
	switch strings.ToLower(c.cfg.HashMech) {
	case "sha512":
		h = sha512.New()
	default:
		h = sha256.New()
	}
	h.Write([]byte(c.cfg.HashNonce))
	h.Write([]byte(username))
	h.Write([]byte{0})
	h.Write([]byte(password))
	digest := h.Sum(nil)

	if c.cfg.HashTruncateBits > 0 {
		digest = truncateRshiftBits(digest, c.cfg.HashTruncateBits)
	}
	return hex.EncodeToString(digest)
}

// truncateRshiftBits keeps the top `bits` of the digest (leading byte
// most significant).
func truncateRshiftBits(b []byte, bits uint) []byte {
	totalBits := uint(len(b)) * 8
	if bits >= totalBits {
		return b
	}
	keepBytes := bits / 8
	remBits := bits % 8
	if remBits == 0 {
		return b[:keepBytes]
	}
	out := make([]byte, keepBytes+1)
	copy(out, b[:keepBytes])
	// Last byte: keep the top `remBits` bits, zero the rest.
	mask := byte(0xff << (8 - remBits))
	out[keepBytes] = b[keepBytes] & mask
	return out
}

// joinCommand appends `command=X`, extending an existing query-string
// when the URL ends with `&`, otherwise starting one with `?`.
func joinCommand(url, command string) string {
	if strings.HasSuffix(url, "&") {
		return url + "command=" + command
	}
	return url + "?command=" + command
}

// splitAPIHeader parses Config.APIHeader. "Key: value" → (Key, value).
// "secret" (no colon) → ("X-API-Key", "secret").
func splitAPIHeader(h string) (key, value string) {
	idx := strings.IndexByte(h, ':')
	if idx < 0 {
		return "X-API-Key", h
	}
	return strings.TrimSpace(h[:idx]), strings.TrimSpace(h[idx+1:])
}
