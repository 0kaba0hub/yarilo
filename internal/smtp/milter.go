package smtp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-milter"
)

// MilterClient connects to a milter server (e.g. rspamd) and runs message checks.
type MilterClient struct {
	client *milter.Client
}

// NewMilterClient creates a MilterClient from a socket spec.
// socket: "unix:/path/to/milter.sock" or "tcp:host:port"
func NewMilterClient(socket string, timeoutSec int) (*MilterClient, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	network, addr, err := parseMilterSocket(socket)
	if err != nil {
		return nil, fmt.Errorf("smtp/milter: %w", err)
	}
	timeout := time.Duration(timeoutSec) * time.Second
	c := milter.NewClientWithOptions(network, addr, milter.ClientOptions{
		Dialer:       &net.Dialer{Timeout: timeout},
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		ActionMask:   milter.OptAddHeader | milter.OptChangeHeader | milter.OptQuarantine,
		ProtocolMask: milter.OptNoConnect | milter.OptNoHelo,
	})
	return &MilterClient{client: c}, nil
}

// Check runs the message through the milter.
// Returns non-nil error if the milter rejects the message.
// Milter unavailability is treated as accept (fail-open).
func (m *MilterClient) Check(ctx context.Context, from string, rcpts []string, body io.Reader) error {
	sess, err := m.client.Session()
	if err != nil {
		slog.Error("smtp/milter: session failed", "err", err)
		return nil // fail-open
	}
	defer sess.Close()

	act, err := sess.Mail(from, nil)
	if err != nil || isReject(act) {
		return fmt.Errorf("milter: rejected MAIL FROM <%s>", from)
	}

	for _, rcpt := range rcpts {
		act, err = sess.Rcpt(rcpt, nil)
		if err != nil || isReject(act) {
			return fmt.Errorf("milter: rejected RCPT TO <%s>", rcpt)
		}
	}

	act, err = sess.HeaderEnd()
	if err != nil || isReject(act) {
		return fmt.Errorf("milter: rejected at header end")
	}

	if body != nil {
		_, act, err = sess.BodyReadFrom(body)
		if err != nil || isReject(act) {
			return fmt.Errorf("milter: rejected message body")
		}
	}

	_, act, err = sess.End()
	if err != nil || isReject(act) {
		return fmt.Errorf("milter: rejected message at end")
	}

	return nil
}

func (m *MilterClient) Close() error {
	return m.client.Close()
}

func isReject(act *milter.Action) bool {
	if act == nil {
		return false
	}
	return act.Code == milter.ActReject || act.Code == milter.ActDiscard || act.Code == milter.ActTempFail
}

func parseMilterSocket(socket string) (network, addr string, err error) {
	switch {
	case strings.HasPrefix(socket, "unix:"):
		return "unix", strings.TrimPrefix(socket, "unix:"), nil
	case strings.HasPrefix(socket, "tcp:"):
		return "tcp", strings.TrimPrefix(socket, "tcp:"), nil
	case strings.HasPrefix(socket, "/"):
		return "unix", socket, nil
	default:
		return "", "", fmt.Errorf("unrecognised socket %q (use unix:/path or tcp:host:port)", socket)
	}
}
