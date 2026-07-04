package sieve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gosieve "github.com/foxcpp/go-sieve/interp"
)

type filterExecutor struct {
	binDir       string
	socketDir    string
	timeout      time.Duration
	crlf         bool
	username     string
	envelopeFrom string
	envelopeTo   string
}

var _ gosieve.FilterExecutor = (*filterExecutor)(nil)

func (e *filterExecutor) Filter(ctx context.Context, programName string, args []string, msg io.Reader) (io.Reader, error) {
	if e.socketDir != "" {
		socketPath := filepath.Join(e.socketDir, programName)
		if fi, err := os.Stat(socketPath); err == nil {
			if fi.Mode()&os.ModeSocket != 0 {
				return e.filterSocket(ctx, socketPath, msg)
			}
		}
	}

	if e.binDir != "" {
		binPath := filepath.Join(e.binDir, programName)
		if fi, err := os.Stat(binPath); err == nil {
			if fi.IsDir() {
				return nil, fmt.Errorf("filter: program %q is a directory", programName)
			}
			if fi.Mode()&0o002 != 0 {
				return nil, fmt.Errorf("filter: program %q is world-writable, refusing to execute", programName)
			}
			return e.filterBin(ctx, binPath, args, msg)
		}
	}

	return nil, nil
}

func (e *filterExecutor) prepareInput(msg io.Reader) (io.Reader, error) {
	if !e.crlf {
		return msg, nil
	}
	data, err := io.ReadAll(msg)
	if err != nil {
		return nil, err
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	return bytes.NewReader(data), nil
}

func (e *filterExecutor) filterBin(ctx context.Context, path string, args []string, msg io.Reader) (io.Reader, error) {
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	input, err := e.prepareInput(msg)
	if err != nil {
		return nil, fmt.Errorf("filter: prepare input: %w", err)
	}

	cmd := exec.CommandContext(tctx, path, args...)
	cmd.Stdin = input
	cmd.Env = []string{
		"USER=" + e.username,
		"SENDER=" + e.envelopeFrom,
		"RECIPIENT=" + e.envelopeTo,
	}
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	if host, err := os.Hostname(); err == nil {
		cmd.Env = append(cmd.Env, "HOST="+host)
	}

	out, err := cmd.Output()
	if err != nil {
		// Non-zero exit = pass-through (false result), not an error.
		if isExitError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("filter: program %q failed: %w", filepath.Base(path), err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return bytes.NewReader(out), nil
}

func (e *filterExecutor) filterSocket(ctx context.Context, socketPath string, msg io.Reader) (io.Reader, error) {
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	input, err := e.prepareInput(msg)
	if err != nil {
		return nil, fmt.Errorf("filter: prepare input: %w", err)
	}

	d := net.Dialer{}
	conn, err := d.DialContext(tctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("filter: connect to socket %q: %w", socketPath, err)
	}
	defer conn.Close()

	if deadline, ok := tctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if _, err := io.Copy(conn, input); err != nil {
		return nil, fmt.Errorf("filter: write to socket %q: %w", socketPath, err)
	}
	if tc, ok := conn.(*net.UnixConn); ok {
		_ = tc.CloseWrite()
	}

	out, err := io.ReadAll(conn)
	if err != nil {
		return nil, fmt.Errorf("filter: read from socket %q: %w", socketPath, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return bytes.NewReader(out), nil
}

func isExitError(err error) bool {
	return strings.Contains(err.Error(), "exit status")
}
