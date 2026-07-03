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

type pipeExecutor struct {
	binDir    string
	socketDir string
	timeout   time.Duration
	crlf      bool
	// envelope fields set per-delivery
	username     string
	envelopeFrom string
	envelopeTo   string
}

var _ gosieve.PipeExecutor = (*pipeExecutor)(nil)

func (e *pipeExecutor) Pipe(ctx context.Context, programName string, args []string, msg io.Reader) error {
	// socket-first, then binary
	if e.socketDir != "" {
		socketPath := filepath.Join(e.socketDir, programName)
		if fi, err := os.Stat(socketPath); err == nil {
			if fi.Mode()&os.ModeSocket != 0 {
				return e.pipeSocket(ctx, socketPath, msg)
			}
		}
	}

	if e.binDir != "" {
		binPath := filepath.Join(e.binDir, programName)
		if fi, err := os.Stat(binPath); err == nil {
			if fi.IsDir() {
				return fmt.Errorf("pipe: program %q is a directory", programName)
			}
			if fi.Mode()&0o002 != 0 {
				return fmt.Errorf("pipe: program %q is world-writable, refusing to execute", programName)
			}
			return e.pipeBin(ctx, binPath, args, msg)
		}
	}

	return fmt.Errorf("pipe: program %q not found in bin_dir or socket_dir", programName)
}

func (e *pipeExecutor) prepareInput(msg io.Reader) (io.Reader, error) {
	if !e.crlf {
		return msg, nil
	}
	// Convert bare LF → CRLF; already-CRLF lines stay intact.
	data, err := io.ReadAll(msg)
	if err != nil {
		return nil, err
	}
	// Normalise: strip existing CR before LF to avoid double CR.
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	return bytes.NewReader(data), nil
}

func (e *pipeExecutor) pipeBin(ctx context.Context, path string, args []string, msg io.Reader) error {
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	input, err := e.prepareInput(msg)
	if err != nil {
		return fmt.Errorf("pipe: prepare input: %w", err)
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

	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("pipe: program %q failed: %w: %s", filepath.Base(path), err, msg)
		}
		return fmt.Errorf("pipe: program %q failed: %w", filepath.Base(path), err)
	}
	return nil
}

func (e *pipeExecutor) pipeSocket(ctx context.Context, socketPath string, msg io.Reader) error {
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	input, err := e.prepareInput(msg)
	if err != nil {
		return fmt.Errorf("pipe: prepare input: %w", err)
	}

	d := net.Dialer{}
	conn, err := d.DialContext(tctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("pipe: connect to socket %q: %w", socketPath, err)
	}
	defer conn.Close()

	if deadline, ok := tctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if _, err := io.Copy(conn, input); err != nil {
		return fmt.Errorf("pipe: write to socket %q: %w", socketPath, err)
	}
	// Signal EOF to the server.
	if tc, ok := conn.(*net.UnixConn); ok {
		_ = tc.CloseWrite()
	}

	// Drain any response (discard — pipe action has no output).
	_, _ = io.Copy(io.Discard, conn)
	return nil
}
