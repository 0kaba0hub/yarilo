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
	"time"

	gosieve "github.com/foxcpp/go-sieve/interp"
)

type executeExecutor struct {
	binDir       string
	socketDir    string
	timeout      time.Duration
	crlf         bool
	username     string
	envelopeFrom string
	envelopeTo   string
}

var _ gosieve.ExecuteExecutor = (*executeExecutor)(nil)

// Execute runs the named program and returns its stdout. ok=true means exit 0.
// input==nil → program receives no stdin.
func (e *executeExecutor) Execute(ctx context.Context, programName string, args []string, input io.Reader) ([]byte, bool, error) {
	if e.socketDir != "" {
		socketPath := filepath.Join(e.socketDir, programName)
		if fi, err := os.Stat(socketPath); err == nil {
			if fi.Mode()&os.ModeSocket != 0 {
				return e.executeSocket(ctx, socketPath, input)
			}
		}
	}

	if e.binDir != "" {
		binPath := filepath.Join(e.binDir, programName)
		if fi, err := os.Stat(binPath); err == nil {
			if fi.IsDir() {
				return nil, false, fmt.Errorf("execute: program %q is a directory", programName)
			}
			if fi.Mode()&0o002 != 0 {
				return nil, false, fmt.Errorf("execute: program %q is world-writable, refusing to execute", programName)
			}
			return e.executeBin(ctx, binPath, args, input)
		}
	}

	return nil, false, fmt.Errorf("execute: program %q not found in bin_dir or socket_dir", programName)
}

func (e *executeExecutor) prepareInput(input io.Reader) (io.Reader, error) {
	if input == nil {
		return nil, nil
	}
	if !e.crlf {
		return input, nil
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	return bytes.NewReader(data), nil
}

func (e *executeExecutor) executeBin(ctx context.Context, path string, args []string, input io.Reader) ([]byte, bool, error) {
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	stdin, err := e.prepareInput(input)
	if err != nil {
		return nil, false, fmt.Errorf("execute: prepare input: %w", err)
	}

	cmd := exec.CommandContext(tctx, path, args...)
	cmd.Stdin = stdin
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
		if isExitError(err) {
			// Non-zero exit: ok=false, output still captured if available.
			var exitErr *exec.ExitError
			if ok2 := asExitError(err, &exitErr); ok2 {
				return exitErr.Stderr, false, nil
			}
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("execute: program %q failed: %w", filepath.Base(path), err)
	}
	return out, true, nil
}

func (e *executeExecutor) executeSocket(ctx context.Context, socketPath string, input io.Reader) ([]byte, bool, error) {
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	stdin, err := e.prepareInput(input)
	if err != nil {
		return nil, false, fmt.Errorf("execute: prepare input: %w", err)
	}

	d := net.Dialer{}
	conn, err := d.DialContext(tctx, "unix", socketPath)
	if err != nil {
		return nil, false, fmt.Errorf("execute: connect to socket %q: %w", socketPath, err)
	}
	defer conn.Close()

	if deadline, ok := tctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if stdin != nil {
		if _, err := io.Copy(conn, stdin); err != nil {
			return nil, false, fmt.Errorf("execute: write to socket %q: %w", socketPath, err)
		}
	}
	if tc, ok := conn.(*net.UnixConn); ok {
		_ = tc.CloseWrite()
	}

	out, err := io.ReadAll(conn)
	if err != nil {
		return nil, false, fmt.Errorf("execute: read from socket %q: %w", socketPath, err)
	}

	// Sockets have no exit code; non-empty output = ok.
	return out, true, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
