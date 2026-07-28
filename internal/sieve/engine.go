package sieve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	gosieve "github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/interp"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// Engine executes Sieve scripts during LMTP delivery.
type Engine struct {
	cfg          config.SieveConfig
	store        ScriptStore
	sender       *Sender
	dupTrackers  sync.Map
	dupDict      dict.Dict    // when non-nil, duplicate dedup is dict-backed (cross-pod with redis)
	locker       locks.Locker // coordinates the file-backed duplicate dedup
	globalBefore []*gosieve.Script
	globalAfter  []*gosieve.Script

	// imapsieve (RFC 6785) global admin scripts, run before/after the
	// mailbox-bound script on each IMAP event.
	imapGlobalBefore []*gosieve.Script
	imapGlobalAfter  []*gosieve.Script
}

// New creates a Sieve Engine. locker is used for cross-process write coordination.
// d is the dict instance for the redis scripts driver; ignored when driver is "fs".
// dupDict backs the duplicate test (RFC 7352); nil falls back to per-process
// in-memory dedup.
func New(cfg config.SieveConfig, locker locks.Locker, d dict.Dict, dupDict dict.Dict) *Engine {
	var s *Sender
	if cfg.SubmissionHost != "" {
		s = newSender(cfg)
	}
	e := &Engine{
		cfg:     cfg,
		store:   NewScriptStore(cfg.ScriptsDriver, cfg.DefaultName, locker, d),
		sender:  s,
		dupDict: dupDict,
		locker:  locker,
	}
	e.globalBefore = loadGlobalScripts(cfg.GlobalBefore)
	e.globalAfter = loadGlobalScripts(cfg.GlobalAfter)
	if cfg.ImapSieveEnabled {
		e.imapGlobalBefore = loadGlobalScripts(cfg.ImapSieveGlobalBefore)
		e.imapGlobalAfter = loadGlobalScripts(cfg.ImapSieveGlobalAfter)
	}
	return e
}

func loadGlobalScripts(paths []string) []*gosieve.Script {
	if len(paths) == 0 {
		return nil
	}
	opts := gosieve.DefaultOptions()
	var scripts []*gosieve.Script
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			slog.Error("sieve: global script read error", "path", p, "err", err)
			continue
		}
		script, err := gosieve.Load(bytes.NewReader(src), opts)
		if err != nil {
			slog.Error("sieve: global script compile error", "path", p, "err", err)
			continue
		}
		scripts = append(scripts, script)
	}
	return scripts
}

// InitUser seeds the active-script pointer with the default keep script on first delivery.
func (e *Engine) InitUser(ctx context.Context, username, homeDir string) error {
	return e.store.InitUser(ctx, username, homeDir)
}

// FilterOptions holds the per-message context passed to Filter.
type FilterOptions struct {
	Username string
	HomeDir  string
	EnvFrom  string
	EnvTo    string
	AuthUser string
	MsgRaw   []byte
	// FolderExists is called by the mailboxexists Sieve test (RFC 5490).
	FolderExists func(ctx context.Context, folder string) (bool, error)
	// MailboxByID resolves a MAILBOXID (RFC 8474 objectid) to the folder name
	// carrying it. Called by fileinto :mailboxid and mailboxidexists (RFC 9042).
	// Returns ("", false) when no folder has that id.
	MailboxByID func(ctx context.Context, id string) (string, bool)
	// MailboxMetadata reads an IMAP METADATA annotation (RFC 5464) on a mailbox,
	// backing the mboxmetadata Sieve tests (RFC 5490 §4). The annotation is the
	// wire-format entry name (/private/… or /shared/…). ("", false, nil) = absent.
	MailboxMetadata func(ctx context.Context, mailbox, annotation string) (string, bool, error)
	// ServerMetadata reads a server-level METADATA annotation, backing the
	// servermetadata Sieve tests. ("", false, nil) = absent.
	ServerMetadata func(ctx context.Context, annotation string) (string, bool, error)
	// Env overrides the Sieve environment provider for this run. Nil uses the
	// default vnd.yarilo.environment provider; imapsieve events pass an imapEnv
	// exposing the RFC 6785 imap.* items.
	Env interp.Env
}

// Filter executes global-before scripts, then the user's active Sieve script,
// then global-after scripts. Returns nil when no scripts are configured —
// the caller applies implicit keep (deliver to INBOX).
// A Reject in any script stops the chain immediately.
func (e *Engine) Filter(ctx context.Context, opts FilterOptions) (*FilterResult, error) {
	hdr := parseHeaders(opts.MsgRaw)
	pol := &policy{
		maxRedirects:    e.cfg.MaxRedirects,
		folderExists:    opts.FolderExists,
		mailboxByID:     opts.MailboxByID,
		mailboxMetadata: opts.MailboxMetadata,
		serverMetadata:  opts.ServerMetadata,
		hdr:             hdr,
		spamHeader:      e.cfg.SpamStatusHeader,
		spamMax:         e.cfg.SpamMaxValue,
		virusHeader:     e.cfg.VirusStatusHeader,
		virusMax:        e.cfg.VirusMaxValue,
	}

	var merged FilterResult
	anyScriptRan := len(e.globalBefore) > 0 || len(e.globalAfter) > 0

	for _, script := range e.globalBefore {
		r, err := e.runScript(ctx, script, opts, hdr, pol)
		if err != nil {
			return nil, err
		}
		if r.Reject != nil {
			return r, nil
		}
		merged.absorb(r)
	}

	src, _, err := e.store.LoadActiveScript(ctx, opts.Username, opts.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("sieve/engine: load script: %w", err)
	}
	if src != nil {
		anyScriptRan = true
		sieveOpts := gosieve.DefaultOptions()
		sieveOpts.Interp.MaxRedirects = e.cfg.MaxRedirects
		sieveOpts.Interp.MaxActions = e.cfg.MaxActions
		sieveOpts.Interp.DebugLog = makeDebugLogger(opts.HomeDir)
		script, err := gosieve.Load(bytes.NewReader(src), sieveOpts)
		if err != nil {
			slog.Error("sieve: script compile error, skipping user script",
				"user", opts.Username, "err", err)
		} else if extErr := CheckExtensions(script, e.cfg.SieveExtensions); extErr != nil {
			slog.Warn("sieve: script uses forbidden extension, skipping",
				"user", opts.Username, "err", extErr)
		} else {
			r, err := e.runScript(ctx, script, opts, hdr, pol)
			if err != nil {
				return nil, err
			}
			if r.Reject != nil {
				return r, nil
			}
			merged.absorb(r)
		}
	}

	for _, script := range e.globalAfter {
		r, err := e.runScript(ctx, script, opts, hdr, pol)
		if err != nil {
			return nil, err
		}
		if r.Reject != nil {
			return r, nil
		}
		merged.absorb(r)
	}

	if !anyScriptRan {
		return nil, nil
	}
	return &merged, nil
}

func (e *Engine) runScript(ctx context.Context, script *gosieve.Script, opts FilterOptions, hdr textproto.MIMEHeader, pol *policy) (*FilterResult, error) {
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

	rd := gosieve.NewRuntimeData(script, pol, env, msg)
	rd.DuplicateTracker = e.dupTracker(opts.Username, opts.HomeDir)
	if opts.Env != nil {
		rd.Env = opts.Env
	} else {
		rd.Env = &yariloEnv{username: opts.Username, configItems: e.cfg.Environments}
	}
	rd.PipeExecutor = &pipeExecutor{
		binDir:    e.cfg.PipeBinDir,
		socketDir: e.cfg.PipeSocketDir,
		timeout: func() time.Duration {
			if e.cfg.PipeExecTimeout > 0 {
				return time.Duration(e.cfg.PipeExecTimeout) * time.Second
			}
			return 10 * time.Second
		}(),
		crlf:         e.cfg.PipeInputEOL != "lf",
		username:     opts.Username,
		envelopeFrom: opts.EnvFrom,
		envelopeTo:   opts.EnvTo,
	}
	rd.FilterExecutor = &filterExecutor{
		binDir:    e.cfg.FilterBinDir,
		socketDir: e.cfg.FilterSocketDir,
		timeout: func() time.Duration {
			if e.cfg.FilterExecTimeout > 0 {
				return time.Duration(e.cfg.FilterExecTimeout) * time.Second
			}
			return 10 * time.Second
		}(),
		crlf:         e.cfg.FilterInputEOL != "lf",
		username:     opts.Username,
		envelopeFrom: opts.EnvFrom,
		envelopeTo:   opts.EnvTo,
	}
	rd.ExecuteExecutor = &executeExecutor{
		binDir:    e.cfg.ExecuteBinDir,
		socketDir: e.cfg.ExecuteSocketDir,
		timeout: func() time.Duration {
			if e.cfg.ExecuteExecTimeout > 0 {
				return time.Duration(e.cfg.ExecuteExecTimeout) * time.Second
			}
			return 10 * time.Second
		}(),
		crlf:         e.cfg.ExecuteInputEOL != "lf",
		username:     opts.Username,
		envelopeFrom: opts.EnvFrom,
		envelopeTo:   opts.EnvTo,
	}
	if err := script.Execute(ctx, rd); err != nil {
		return nil, fmt.Errorf("sieve/engine: execute: %w", err)
	}

	if len(rd.FilteredMessage) > 0 {
		opts.MsgRaw = rd.FilteredMessage
		slog.Info("sieve: filter substituted message", "user", opts.Username, "bytes", len(opts.MsgRaw))
	}

	result := buildResult(rd)
	if len(rd.FilteredMessage) > 0 {
		result.Message = rd.FilteredMessage
	}

	pe := rd.PipeExecutor
	for _, p := range result.Pipes {
		if err := pe.Pipe(ctx, p.ProgramName, p.Args, bytes.NewReader(opts.MsgRaw)); err != nil {
			if p.Try {
				slog.Info("sieve: pipe failed (try)", "user", opts.Username, "program", p.ProgramName, "err", err)
			} else {
				return nil, fmt.Errorf("sieve/engine: pipe %q: %w", p.ProgramName, err)
			}
		} else if p.Try && !p.Copy {
			// Successful :try non-copy pipe takes over delivery; cancel implicit keep.
			result.Deliveries = removeImplicitKeep(result.Deliveries)
		}
	}

	if e.sender != nil {
		for _, r := range result.Redirects {
			if err := e.sender.sendRedirect(ctx, opts.EnvFrom, r.Address, opts.MsgRaw); err != nil {
				slog.Error("sieve: redirect failed", "user", opts.Username, "to", r.Address, "err", err)
			}
		}
		for _, resp := range result.VacationReplies {
			if err := e.sender.sendVacation(ctx, e.store, opts, hdr, resp); err != nil {
				slog.Error("sieve: vacation failed", "user", opts.Username, "err", err)
			}
		}
		for _, n := range result.Notifications {
			if err := e.sender.sendNotify(ctx, opts, hdr, n); err != nil {
				slog.Error("sieve: notify failed", "user", opts.Username, "method", n.Method, "err", err)
			}
		}
		for _, rep := range result.Reports {
			if err := e.sender.sendReport(ctx, opts, rep); err != nil {
				slog.Error("sieve: report failed", "user", opts.Username, "to", rep.Target, "err", err)
			}
		}
	}

	return result, nil
}

func parseHeaders(raw []byte) textproto.MIMEHeader {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	hdr, _ := r.ReadMIMEHeader()
	return hdr
}

// policy satisfies interp.SpamVirusChecker so the spamtest / virustest
// extensions resolve against the configured status headers.
var _ interp.SpamVirusChecker = (*policy)(nil)

type policy struct {
	maxRedirects    int
	folderExists    func(ctx context.Context, folder string) (bool, error)
	mailboxByID     func(ctx context.Context, id string) (string, bool)
	mailboxMetadata func(ctx context.Context, mailbox, annotation string) (string, bool, error)
	serverMetadata  func(ctx context.Context, annotation string) (string, bool, error)

	// Spam/virus test backing (RFC 5235). When the configured header is empty
	// or absent from the message, the test reports "not scanned" (tested=false).
	hdr         textproto.MIMEHeader
	spamHeader  string
	spamMax     float64
	virusHeader string
	virusMax    float64
}

// SpamScore implements interp.SpamVirusChecker: it reads the configured spam
// header and normalises the raw value against spamMax. A tested message maps
// onto the graded 1..10 scale (RFC 5235 §2.1 — "0" is reserved for "not
// tested"); with :percent (spamtestplus) it maps onto 0..100 instead.
func (p *policy) SpamScore(_ context.Context, percent bool) (string, bool) {
	return normalizeScore(p.hdr, p.spamHeader, p.spamMax, percent, 10)
}

// VirusScore implements interp.SpamVirusChecker: it reads the configured virus
// header and normalises onto the graded 1..5 scale (RFC 5235 §3.1 — "0" is
// reserved for "not tested", 1 = clean, 5 = definitely a virus).
func (p *policy) VirusScore(_ context.Context) (string, bool) {
	return normalizeScore(p.hdr, p.virusHeader, p.virusMax, false, 5)
}

// normalizeScore reads header from hdr, parses its leading numeric value, and
// maps the raw/max ratio onto the RFC 5235 scale: a *tested* message grades
// onto 1..maxGrade (1 = clean, maxGrade = certain), matching the reference's
// `1 + rint(norm*(maxGrade-1))`; with percent it maps onto 0..100. "0" is
// reserved for "not tested", so an untested / unconfigured / unparsable header
// returns ("0", false) — never a tested value of 0.
func normalizeScore(hdr textproto.MIMEHeader, header string, max float64, percent bool, maxGrade int) (string, bool) {
	if header == "" || hdr == nil {
		return "0", false
	}
	raw := hdr.Get(header)
	if raw == "" {
		return "0", false
	}
	val, err := parseLeadingFloat(raw)
	if err != nil {
		return "0", false
	}
	if max <= 0 {
		max = float64(maxGrade)
	}
	ratio := val / max
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	if percent {
		return strconv.Itoa(int(math.Round(ratio * 100))), true
	}
	return strconv.Itoa(int(math.Round(1 + ratio*float64(maxGrade-1)))), true
}

// parseLeadingFloat extracts the leading signed float from s (e.g. "5.3 / 10"
// → 5.3), tolerating trailing text that spam scanners append.
func parseLeadingFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0, fmt.Errorf("sieve: no numeric prefix in %q", s)
	}
	return strconv.ParseFloat(s[:end], 64)
}

func (p *policy) RedirectAllowed(_ context.Context, d *interp.RuntimeData, _ string) (bool, error) {
	count := 0
	for _, a := range d.AppliedActions {
		if r, ok := a.(interp.ActionRedirect); ok && !r.Copy {
			count++
		}
	}
	return count < p.maxRedirects, nil
}

func (p *policy) MailboxExists(ctx context.Context, folder string) (bool, error) {
	if p.folderExists == nil {
		return false, nil
	}
	return p.folderExists(ctx, folder)
}

// MailboxByID implements interp.MailboxIDChecker (RFC 9042): it resolves a
// MAILBOXID to the folder that carries it, backing fileinto :mailboxid and the
// mailboxidexists test. Returns ("", false) when unresolved or unavailable.
func (p *policy) MailboxByID(ctx context.Context, id string) (string, bool) {
	if p.mailboxByID == nil {
		return "", false
	}
	return p.mailboxByID(ctx, id)
}

// GetMailboxMetadata implements interp.MetadataChecker (RFC 5490 §4): it reads a
// mailbox-scoped IMAP METADATA annotation, backing the mboxmetadata tests.
func (p *policy) GetMailboxMetadata(ctx context.Context, mailbox, annotation string) (string, bool, error) {
	if p.mailboxMetadata == nil {
		return "", false, nil
	}
	return p.mailboxMetadata(ctx, mailbox, annotation)
}

// GetServerMetadata implements interp.MetadataChecker (RFC 5490 §4): it reads a
// server-scoped IMAP METADATA annotation, backing the servermetadata tests.
func (p *policy) GetServerMetadata(ctx context.Context, annotation string) (string, bool, error) {
	if p.serverMetadata == nil {
		return "", false, nil
	}
	return p.serverMetadata(ctx, annotation)
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
		case interp.ActionFileInto:
			result.Deliveries = append(result.Deliveries, Delivery{
				Folder:     a.Mailbox,
				Flags:      []string(a.Flags),
				Create:     a.Create,
				SpecialUse: a.SpecialUse,
			})
		case interp.ActionKeep:
			result.Deliveries = append(result.Deliveries, Delivery{
				Folder:   "INBOX",
				Flags:    []string(a.Flags),
				Implicit: a.Implicit,
				FromKeep: true,
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
		case interp.ActionReport:
			result.Reports = append(result.Reports, a)
		case interp.ActionPipe:
			result.Pipes = append(result.Pipes, PipeAction{
				ProgramName: a.ProgramName,
				Args:        a.Args,
				Copy:        a.Copy,
				Try:         a.Try,
			})
		}
	}
	for _, resp := range d.VacationResponses {
		result.VacationReplies = append(result.VacationReplies, resp)
	}
	return result
}

func (r *FilterResult) absorb(other *FilterResult) {
	if other == nil {
		return
	}
	r.Deliveries = append(r.Deliveries, other.Deliveries...)
	r.Redirects = append(r.Redirects, other.Redirects...)
	r.Pipes = append(r.Pipes, other.Pipes...)
	r.VacationReplies = append(r.VacationReplies, other.VacationReplies...)
	r.Notifications = append(r.Notifications, other.Notifications...)
	r.Reports = append(r.Reports, other.Reports...)
	if other.Message != nil {
		r.Message = other.Message
	}
}

func removeImplicitKeep(deliveries []Delivery) []Delivery {
	out := deliveries[:0]
	for _, d := range deliveries {
		if !d.Implicit {
			out = append(out, d)
		}
	}
	return out
}

// dupTracker returns the duplicate-test backend for a user, selected by
// sieve_duplicate_driver: "file" (home-dir file, cross-pod on shared storage),
// "memory" (per-process), or "redis" (the sieve_duplicate dict). A "file"
// driver with no home directory (unit tests) falls back to memory, as does a
// "redis" driver with no configured dict.
func (e *Engine) dupTracker(username, homeDir string) interp.DuplicateTracker {
	inner := e.dupTrackerBackend(username, homeDir)
	return clampedDuplicateTracker{inner: inner, maxSeconds: uint32(e.cfg.DuplicateMaxPeriod)} //nolint:gosec
}

func (e *Engine) dupTrackerBackend(username, homeDir string) interp.DuplicateTracker {
	switch e.cfg.DuplicateDriver {
	case "redis":
		if e.dupDict != nil {
			return NewDictDuplicateTracker(e.dupDict, username)
		}
		slog.Warn("sieve: sieve_duplicate_driver=redis but no sieve_duplicate dict; using in-memory dedup", "user", username)
	case "memory":
		// handled by the shared fallback below
	default: // "file" (and empty)
		if homeDir != "" {
			return NewFileDuplicateTracker(homeDir, e.cfg.DuplicateFile, e.locker)
		}
	}
	fresh := interp.NewMemoryDuplicateTracker()
	v, _ := e.dupTrackers.LoadOrStore(username, fresh)
	if t, ok := v.(*interp.MemoryDuplicateTracker); ok {
		return t
	}
	return fresh
}
