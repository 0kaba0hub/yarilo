package sieve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/textproto"

	gosieve "github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/interp"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// Engine executes Sieve scripts during LMTP delivery.
type Engine struct {
	cfg  config.SieveConfig
	dict dict.Dict
}

// New creates a Sieve Engine backed by the given dict for script storage.
func New(cfg config.SieveConfig, d dict.Dict) *Engine {
	return &Engine{cfg: cfg, dict: d}
}

// FilterOptions holds the per-message context passed to Filter.
type FilterOptions struct {
	Username string
	HomeDir  string
	EnvFrom  string
	EnvTo    string
	AuthUser string
	// MsgRaw is the complete raw RFC 2822 message (headers + body).
	MsgRaw []byte
	// FolderExists is called by the mailboxexists Sieve test (RFC 5490).
	// May be nil — the test returns false when not provided.
	FolderExists func(ctx context.Context, folder string) (bool, error)
}

// Filter executes the active Sieve script for the user and returns the result.
// Returns nil when no active script is configured — the caller should apply
// implicit keep (deliver to INBOX).
func (e *Engine) Filter(ctx context.Context, opts FilterOptions) (*FilterResult, error) {
	src, _, err := loadActiveScript(ctx, e.dict, opts.Username, opts.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("sieve/engine: load script: %w", err)
	}
	if src == nil {
		return nil, nil
	}

	sieveOpts := gosieve.DefaultOptions()
	sieveOpts.Interp.MaxRedirects = e.cfg.MaxRedirects

	script, err := gosieve.Load(bytes.NewReader(src), sieveOpts)
	if err != nil {
		slog.Error("sieve: script compile error, falling back to implicit keep",
			"user", opts.Username, "err", err)
		return nil, nil
	}

	hdr := parseHeaders(opts.MsgRaw)
	msg := interp.MessageStatic{
		Size:       len(opts.MsgRaw),
		Header:     hdr,
		RawMessage: opts.MsgRaw,
	}
	env := interp.EnvelopeStatic{
		From: opts.EnvFrom,
		To:   opts.EnvTo,
		Auth: opts.AuthUser,
	}
	pol := &policy{
		maxRedirects: e.cfg.MaxRedirects,
		folderExists: opts.FolderExists,
	}

	d := gosieve.NewRuntimeData(script, pol, env, msg)
	if err := script.Execute(ctx, d); err != nil {
		return nil, fmt.Errorf("sieve/engine: execute: %w", err)
	}

	return buildResult(d), nil
}

// parseHeaders parses RFC 2822 headers from a raw message.
// Returns whatever headers were parsed; never returns a hard error.
func parseHeaders(raw []byte) textproto.MIMEHeader {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	hdr, _ := r.ReadMIMEHeader()
	return hdr
}

// policy implements interp.PolicyReader and interp.MailboxChecker.
type policy struct {
	maxRedirects int
	folderExists func(ctx context.Context, folder string) (bool, error)
}

// RedirectAllowed implements interp.PolicyReader.
func (p *policy) RedirectAllowed(_ context.Context, d *interp.RuntimeData, _ string) (bool, error) {
	count := 0
	for _, a := range d.AppliedActions {
		if r, ok := a.(interp.ActionRedirect); ok && !r.Copy {
			count++
		}
	}
	return count < p.maxRedirects, nil
}

// MailboxExists implements interp.MailboxChecker (mailboxexists test, RFC 5490).
func (p *policy) MailboxExists(ctx context.Context, folder string) (bool, error) {
	if p.folderExists == nil {
		return false, nil
	}
	return p.folderExists(ctx, folder)
}

func buildResult(d *interp.RuntimeData) *FilterResult {
	result := &FilterResult{}

	for _, act := range d.AppliedActions {
		switch a := act.(type) {
		case interp.ActionReject:
			return &FilterResult{Reject: &RejectErr{Reason: a.Reason}}
		case interp.ActionEReject:
			return &FilterResult{Reject: &RejectErr{Enhanced: true, Reason: a.Reason}}
		case interp.ActionDiscard:
			// Deliveries stays nil — message is discarded.
		case interp.ActionFileInto:
			result.Deliveries = append(result.Deliveries, Delivery{
				Folder:     a.Mailbox,
				Flags:      []string(a.Flags),
				Create:     a.Create,
				SpecialUse: a.SpecialUse,
			})
		case interp.ActionKeep:
			result.Deliveries = append(result.Deliveries, Delivery{
				Folder: "INBOX",
				Flags:  []string(a.Flags),
			})
		case interp.ActionRedirect:
			if a.ListName != "" {
				// extlists list-redirect deferred to Phase 2.
				continue
			}
			result.Redirects = append(result.Redirects, Redirect{
				Address: a.Address,
				Copy:    a.Copy,
			})
		}
	}

	for _, resp := range d.VacationResponses {
		result.VacationReplies = append(result.VacationReplies, resp)
	}

	return result
}
