package managesieve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"unicode"
	"unicode/utf8"

	gosieve "github.com/foxcpp/go-sieve"

	"github.com/0kaba0hub/yarilo/internal/sieve"
)

type session struct {
	conn              net.Conn
	r                 *bufio.Reader
	w                 *bufio.Writer
	username          string
	homeDir           string
	store             sieve.ScriptStore
	maxSize           int
	allowedExtensions []string
	// sid is the anvil session correlation ID from the login-proxy preamble
	// (loginproto.PreambleConn.SessionID) — included on every command
	// dispatch and error line so a session's activity greps as one thread.
	sid string
}

func (s *session) serve(ctx context.Context) {
	defer s.conn.Close()

	if err := writeCapabilities(s.w, sieve.EffectiveExtensions(s.allowedExtensions)); err != nil {
		return
	}
	if err := writeOK(s.w, "ManageSieve ready."); err != nil {
		return
	}
	if err := s.w.Flush(); err != nil {
		return
	}

	for {
		if ctx.Err() != nil {
			_ = writeBYE(s.w, "Server shutting down.")
			_ = s.w.Flush()
			return
		}

		cmd, err := readAtom(s.r)
		if err != nil {
			return
		}
		if cmd == "" {
			skipLine(s.r)
			continue
		}
		slog.Debug("managesieve: command", "sid", s.sid, "user", s.username, "cmd", cmd)

		switch cmd {
		case "CAPABILITY":
			skipLine(s.r)
			s.handleCapability()
		case "LISTSCRIPTS":
			skipLine(s.r)
			s.handleListScripts(ctx)
		case "PUTSCRIPT":
			s.handlePutScript(ctx)
		case "GETSCRIPT":
			s.handleGetScript(ctx)
		case "SETACTIVE":
			s.handleSetActive(ctx)
		case "DELETESCRIPT":
			s.handleDeleteScript(ctx)
		case "CHECKSCRIPT":
			s.handleCheckScript()
		case "RENAMESCRIPT":
			s.handleRenameScript(ctx)
		case "NOOP":
			s.handleNoop()
		case "LOGOUT":
			skipLine(s.r)
			_ = writeBYE(s.w, "Logging out.")
			_ = s.w.Flush()
			return
		default:
			skipLine(s.r)
			_ = writeNO(s.w, "", fmt.Sprintf("Unknown command: %s", cmd))
		}
		if err := s.w.Flush(); err != nil {
			return
		}
	}
}

func (s *session) handleCapability() {
	if err := writeCapabilities(s.w, sieve.EffectiveExtensions(s.allowedExtensions)); err != nil {
		return
	}
	_ = writeOK(s.w, "CAPABILITY completed.")
}

func (s *session) handleListScripts(ctx context.Context) {
	names, err := s.store.ListScripts(ctx, s.username, s.homeDir)
	if err != nil {
		slog.Error("managesieve: list scripts", "sid", s.sid, "user", s.username, "err", err)
		_ = writeNO(s.w, "", "Server error listing scripts.")
		return
	}
	active, err := s.store.ActiveScriptName(ctx, s.username, s.homeDir)
	if err != nil {
		slog.Error("managesieve: active script", "sid", s.sid, "user", s.username, "err", err)
		_ = writeNO(s.w, "", "Server error reading active script name.")
		return
	}
	for _, name := range names {
		if name == active {
			if _, err := fmt.Fprintf(s.w, "%s ACTIVE\r\n", quoteStr(name)); err != nil {
				return
			}
		} else {
			if _, err := fmt.Fprintf(s.w, "%s\r\n", quoteStr(name)); err != nil {
				return
			}
		}
	}
	_ = writeOK(s.w, "LISTSCRIPTS completed.")
}

func (s *session) handlePutScript(ctx context.Context) {
	name, err := readString(s.r, nil)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad script name.")
		return
	}
	cont := func() error { return writeContinue(s.w) }
	src, err := readLastArg(s.r, cont)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad script content.")
		return
	}

	if s.maxSize > 0 && len(src) > s.maxSize {
		_ = writeNO(s.w, "QUOTA/MAXSCRIPTSIZE", "Script too large.")
		return
	}

	nameStr := string(name)
	if nameStr == s.store.DefaultScriptName() {
		_ = writeNO(s.w, "", "Script name is reserved.")
		return
	}

	compiled, err := gosieve.Load(bytes.NewReader(src), gosieve.DefaultOptions())
	if err != nil {
		_ = writeNO(s.w, "", fmt.Sprintf("Script error: %s", strings.TrimSpace(err.Error())))
		return
	}
	if extErr := sieve.CheckExtensions(compiled, s.allowedExtensions); extErr != nil {
		_ = writeNO(s.w, "FORBIDDEN", ucFirst(extErr.Error())+".")
		return
	}

	if err := s.store.SaveScript(ctx, s.username, s.homeDir, nameStr, src); err != nil {
		slog.Error("managesieve: put script", "sid", s.sid, "user", s.username, "script", nameStr, "err", err)
		_ = writeNO(s.w, "", "Server error storing script.")
		return
	}
	slog.Info("managesieve: script stored", "sid", s.sid, "user", s.username, "script", nameStr, "bytes", len(src))
	_ = writeOK(s.w, "PUTSCRIPT completed.")
}

func (s *session) handleGetScript(ctx context.Context) {
	name, err := readLastArg(s.r, nil)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad script name.")
		return
	}

	nameStr := string(name)
	src, found, err := s.store.GetScript(ctx, s.username, s.homeDir, nameStr)
	if err != nil {
		slog.Error("managesieve: get script", "sid", s.sid, "user", s.username, "script", nameStr, "err", err)
		_ = writeNO(s.w, "", "Server error retrieving script.")
		return
	}
	if !found {
		_ = writeNO(s.w, "NONEXISTENT", "Script does not exist.")
		return
	}
	if err := writeLiteral(s.w, src); err != nil {
		return
	}
	_, _ = s.w.WriteString(crlf)
	_ = writeOK(s.w, "GETSCRIPT completed.")
}

func (s *session) handleSetActive(ctx context.Context) {
	name, err := readLastArg(s.r, nil)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad script name.")
		return
	}

	nameStr := string(name)
	if nameStr == "" {
		if err := s.store.Deactivate(ctx, s.username, s.homeDir); err != nil {
			slog.Error("managesieve: deactivate", "sid", s.sid, "user", s.username, "err", err)
			_ = writeNO(s.w, "", "Server error deactivating script.")
			return
		}
		slog.Info("managesieve: script deactivated", "sid", s.sid, "user", s.username)
		_ = writeOK(s.w, "SETACTIVE completed.")
		return
	}

	if nameStr == s.store.DefaultScriptName() {
		_ = writeNO(s.w, "NONEXISTENT", "Script does not exist.")
		return
	}

	_, found, err := s.store.GetScript(ctx, s.username, s.homeDir, nameStr)
	if err != nil {
		slog.Error("managesieve: setactive check", "sid", s.sid, "user", s.username, "err", err)
		_ = writeNO(s.w, "", "Server error.")
		return
	}
	if !found {
		_ = writeNO(s.w, "NONEXISTENT", "Script does not exist.")
		return
	}

	if err := s.store.SetActive(ctx, s.username, s.homeDir, nameStr); err != nil {
		slog.Error("managesieve: set active", "sid", s.sid, "user", s.username, "script", nameStr, "err", err)
		_ = writeNO(s.w, "", "Server error activating script.")
		return
	}
	slog.Info("managesieve: script activated", "sid", s.sid, "user", s.username, "script", nameStr)
	_ = writeOK(s.w, "SETACTIVE completed.")
}

func (s *session) handleDeleteScript(ctx context.Context) {
	name, err := readLastArg(s.r, nil)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad script name.")
		return
	}

	nameStr := string(name)
	if nameStr == s.store.DefaultScriptName() {
		_ = writeNO(s.w, "", "Cannot delete reserved script.")
		return
	}

	_, found, err := s.store.GetScript(ctx, s.username, s.homeDir, nameStr)
	if err != nil {
		slog.Error("managesieve: delete check", "sid", s.sid, "user", s.username, "err", err)
		_ = writeNO(s.w, "", "Server error.")
		return
	}
	if !found {
		_ = writeNO(s.w, "NONEXISTENT", "Script does not exist.")
		return
	}

	active, err := s.store.ActiveScriptName(ctx, s.username, s.homeDir)
	if err != nil {
		slog.Error("managesieve: delete active check", "sid", s.sid, "user", s.username, "err", err)
		_ = writeNO(s.w, "", "Server error.")
		return
	}
	if active == nameStr {
		_ = writeNO(s.w, "ACTIVE", "Cannot delete the active script.")
		return
	}

	if err := s.store.DeleteScript(ctx, s.username, s.homeDir, nameStr); err != nil {
		slog.Error("managesieve: delete script", "sid", s.sid, "user", s.username, "script", nameStr, "err", err)
		_ = writeNO(s.w, "", "Server error deleting script.")
		return
	}
	slog.Info("managesieve: script deleted", "sid", s.sid, "user", s.username, "script", nameStr)
	_ = writeOK(s.w, "DELETESCRIPT completed.")
}

func (s *session) handleCheckScript() {
	cont := func() error { return writeContinue(s.w) }
	src, err := readLastArg(s.r, cont)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad script content.")
		return
	}

	if s.maxSize > 0 && len(src) > s.maxSize {
		_ = writeNO(s.w, "QUOTA/MAXSCRIPTSIZE", "Script too large.")
		return
	}

	compiled, err := gosieve.Load(bytes.NewReader(src), gosieve.DefaultOptions())
	if err != nil {
		_ = writeNO(s.w, "", fmt.Sprintf("Script error: %s", strings.TrimSpace(err.Error())))
		return
	}
	if extErr := sieve.CheckExtensions(compiled, s.allowedExtensions); extErr != nil {
		_ = writeNO(s.w, "FORBIDDEN", ucFirst(extErr.Error())+".")
		return
	}
	_ = writeOK(s.w, "CHECKSCRIPT completed.")
}

func (s *session) handleRenameScript(ctx context.Context) {
	oldName, err := readString(s.r, nil)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad old script name.")
		return
	}
	newName, err := readLastArg(s.r, nil)
	if err != nil {
		skipLine(s.r)
		_ = writeNO(s.w, "", "Bad new script name.")
		return
	}

	oldStr, newStr := string(oldName), string(newName)

	if oldStr == s.store.DefaultScriptName() || newStr == s.store.DefaultScriptName() {
		_ = writeNO(s.w, "", "Script name is reserved.")
		return
	}

	_, found, err := s.store.GetScript(ctx, s.username, s.homeDir, oldStr)
	if err != nil {
		slog.Error("managesieve: rename get", "sid", s.sid, "user", s.username, "err", err)
		_ = writeNO(s.w, "", "Server error.")
		return
	}
	if !found {
		_ = writeNO(s.w, "NONEXISTENT", "Script does not exist.")
		return
	}

	_, newExists, err := s.store.GetScript(ctx, s.username, s.homeDir, newStr)
	if err != nil {
		_ = writeNO(s.w, "", "Server error.")
		return
	}
	if newExists {
		_ = writeNO(s.w, "ALREADYEXISTS", "Target script already exists.")
		return
	}

	if err := s.store.RenameScript(ctx, s.username, s.homeDir, oldStr, newStr); err != nil {
		slog.Error("managesieve: rename script", "sid", s.sid, "user", s.username, "old", oldStr, "new", newStr, "err", err)
		_ = writeNO(s.w, "", "Server error renaming script.")
		return
	}

	slog.Info("managesieve: script renamed", "sid", s.sid, "user", s.username, "old", oldStr, "new", newStr)
	_ = writeOK(s.w, "RENAMESCRIPT completed.")
}

func (s *session) handleNoop() {
	skipWS(s.r)
	b, err := s.r.ReadByte()
	if err != nil || b == '\r' || b == '\n' {
		if err == nil && b == '\r' {
			_, _ = s.r.ReadByte()
		}
		_ = writeOK(s.w, "")
		return
	}
	_ = s.r.UnreadByte()

	tag, err := readLastArg(s.r, nil)
	if err != nil {
		_ = writeOK(s.w, "")
		return
	}
	_ = writeOK(s.w, string(tag))
}

func ucFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
