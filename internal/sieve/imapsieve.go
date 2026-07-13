package sieve

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	gosieve "github.com/foxcpp/go-sieve"
)

// IMAPEventOptions is the context for an imapsieve (RFC 6785) run triggered by an
// IMAP event on a stored message.
type IMAPEventOptions struct {
	Username     string
	HomeDir      string
	Cause        string   // "APPEND", "COPY", or "FLAG"
	Mailbox      string   // affected (destination) mailbox
	SrcMailbox   string   // COPY/MOVE source mailbox (empty otherwise)
	ChangedFlags []string // FLAG cause: the flags that changed
	MsgRaw       []byte
	ScriptName   string // mailbox-bound script name (from METADATA); "" = globals only
}

// RunIMAPEvent runs the imapsieve script chain — admin global-before → the
// mailbox-bound script (named by the /shared/imapsieve/script METADATA
// annotation) → admin global-after — for an IMAP event, returning the merged
// actions. Returns (nil, nil) when imapsieve is disabled or no script applies.
func (e *Engine) RunIMAPEvent(ctx context.Context, opts IMAPEventOptions) (*FilterResult, error) {
	if !e.cfg.ImapSieveEnabled {
		return nil, nil
	}
	hdr := parseHeaders(opts.MsgRaw)
	pol := &policy{
		maxRedirects: e.cfg.MaxRedirects,
		hdr:          hdr,
		spamHeader:   e.cfg.SpamStatusHeader,
		spamMax:      e.cfg.SpamMaxValue,
		virusHeader:  e.cfg.VirusStatusHeader,
		virusMax:     e.cfg.VirusMaxValue,
	}
	fopts := FilterOptions{
		Username: opts.Username,
		HomeDir:  opts.HomeDir,
		EnvTo:    opts.Username,
		MsgRaw:   opts.MsgRaw,
		Env: &imapEnv{
			base:         &yariloEnv{username: opts.Username, configItems: e.cfg.Environments},
			cause:        opts.Cause,
			mailbox:      opts.Mailbox,
			email:        opts.Username,
			changedFlags: strings.Join(opts.ChangedFlags, " "),
			fromMailbox:  opts.SrcMailbox,
			toMailbox:    opts.Mailbox,
		},
	}

	var merged FilterResult
	ran := false
	run := func(script *gosieve.Script) error {
		r, err := e.runScript(ctx, script, fopts, hdr, pol)
		if err != nil {
			return err
		}
		ran = true
		merged.absorb(r)
		return nil
	}

	for _, s := range e.imapGlobalBefore {
		if err := run(s); err != nil {
			return nil, err
		}
	}
	if opts.ScriptName != "" {
		bound, err := e.loadImapScript(opts.ScriptName)
		if err != nil {
			slog.Warn("imapsieve: load bound script", "name", opts.ScriptName, "err", err)
		} else if bound != nil {
			if err := run(bound); err != nil {
				return nil, err
			}
		}
	}
	for _, s := range e.imapGlobalAfter {
		if err := run(s); err != nil {
			return nil, err
		}
	}
	if !ran {
		return nil, nil
	}
	return &merged, nil
}

// ImapSieveEnabled reports whether imapsieve is active.
func (e *Engine) ImapSieveEnabled() bool { return e.cfg.ImapSieveEnabled }

// HasImapGlobals reports whether any admin global-before/after imapsieve script
// is configured — so a caller can skip per-message work when nothing would run
// without a mailbox-bound script.
func (e *Engine) HasImapGlobals() bool {
	return len(e.imapGlobalBefore) > 0 || len(e.imapGlobalAfter) > 0
}

// loadImapScript compiles the named admin script from imapsieve_script_dir
// (<dir>/<name>.sieve). A missing dir or file returns (nil, nil) — the event
// proceeds with only the global scripts.
func (e *Engine) loadImapScript(name string) (*gosieve.Script, error) {
	if e.cfg.ImapSieveScriptDir == "" || name == "" {
		return nil, nil
	}
	// The name comes from client-set METADATA — reject path traversal.
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("imapsieve: invalid script name %q", name)
	}
	path := filepath.Join(e.cfg.ImapSieveScriptDir, name+".sieve")
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sieveOpts := gosieve.DefaultOptions()
	sieveOpts.Interp.MaxRedirects = e.cfg.MaxRedirects
	sieveOpts.Interp.MaxActions = e.cfg.MaxActions
	script, err := gosieve.Load(bytes.NewReader(src), sieveOpts)
	if err != nil {
		return nil, fmt.Errorf("imapsieve: compile %s: %w", name, err)
	}
	if extErr := CheckExtensions(script, e.cfg.SieveExtensions); extErr != nil {
		return nil, fmt.Errorf("imapsieve: %w", extErr)
	}
	return script, nil
}
