package sieve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/textproto"
	"sync"

	gosieve "github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/interp"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// Engine executes Sieve scripts during LMTP delivery.
type Engine struct {
	cfg         config.SieveConfig
	dict        dict.Dict
	sender      *Sender
	dupTrackers sync.Map // username → *interp.MemoryDuplicateTracker
}

// New creates a Sieve Engine backed by the given dict for script storage.
// When cfg.SubmissionHost is non-empty, outbound mail for redirect and
// vacation actions is dispatched via an SMTP client to that host.
func New(cfg config.SieveConfig, d dict.Dict) *Engine {
	var s *Sender
	if cfg.SubmissionHost != "" {
		s = newSender(cfg)
	}
	return &Engine{cfg: cfg, dict: d, sender: s}
}

// InitUser seeds the default yarilo.sieve script for a user on first delivery.
// No-op when the user already has an active script configured.
func (e *Engine) InitUser(ctx context.Context, username, homeDir string) error {
	return InitUser(ctx, e.dict, username, homeDir)
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
	d.DuplicateTracker = e.dupTracker(opts.Username)
	if err := script.Execute(ctx, d); err != nil {
		return nil, fmt.Errorf("sieve/engine: execute: %w", err)
	}

	result := buildResult(d)

	if e.sender != nil {
		for _, r := range result.Redirects {
			if err := e.sender.sendRedirect(ctx, opts.EnvFrom, r.Address, opts.MsgRaw); err != nil {
				slog.Error("sieve: redirect failed", "user", opts.Username, "to", r.Address, "err", err)
			}
		}
		for _, resp := range result.VacationReplies {
			if err := e.sender.sendVacation(ctx, e.dict, opts, hdr, resp); err != nil {
				slog.Error("sieve: vacation failed", "user", opts.Username, "err", err)
			}
		}
		for _, n := range result.Notifications {
			if err := e.sender.sendNotify(ctx, opts, n); err != nil {
				slog.Error("sieve: notify failed", "user", opts.Username, "method", n.Method, "err", err)
			}
		}
	}

	return result, nil
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
				continue
			}
			result.Redirects = append(result.Redirects, Redirect{
				Address: a.Address,
				Copy:    a.Copy,
			})
		case interp.ActionNotify:
			result.Notifications = append(result.Notifications, a)
		}
	}

	for _, resp := range d.VacationResponses {
		result.VacationReplies = append(result.VacationReplies, resp)
	}

	return result
}

func (e *Engine) dupTracker(username string) *interp.MemoryDuplicateTracker {
	fresh := interp.NewMemoryDuplicateTracker()
	v, _ := e.dupTrackers.LoadOrStore(username, fresh)
	if t, ok := v.(*interp.MemoryDuplicateTracker); ok {
		return t
	}
	return fresh
}
