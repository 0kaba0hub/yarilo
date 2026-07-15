package lmtp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	goSmtp "github.com/emersion/go-smtp"
)

// stripRcptPrefix removes a leading "<rcpt> " that the backend LMTP server
// prepends to each per-recipient DATA reply. The login server prepends its own
// "<rcpt> " when relaying, so without stripping here the address appears twice
// (e.g. "452 4.2.2 <u@x> <u@x> Mailbox full"). Returns e unchanged when the
// prefix is absent.
func stripRcptPrefix(e *goSmtp.SMTPError, rcpt string) *goSmtp.SMTPError {
	prefix := "<" + rcpt + "> "
	if e == nil || !strings.HasPrefix(e.Message, prefix) {
		return e
	}
	clone := *e
	clone.Message = strings.TrimPrefix(e.Message, prefix)
	return &clone
}

// UserRouter resolves a recipient username to a backend IP address.
// Implementations consult admin overrides, sticky routing, and the
// consistent-hash ring in that order.
type UserRouter interface {
	RouteUser(username string) (ip string, err error)
}

// proxyRouter resolves recipient usernames to backend LMTP addresses.
// Only active on director nodes.
type proxyRouter struct {
	router      UserRouter
	backendPort int
	timeout     time.Duration
	hostname    string // LHLO name sent to backend
}

func newProxyRouter(hostname string, router UserRouter, backendPort int, timeout time.Duration) *proxyRouter {
	if backendPort == 0 {
		backendPort = 24
	}
	return &proxyRouter{router: router, backendPort: backendPort, timeout: timeout, hostname: hostname}
}

// route returns the backend TCP address for a recipient username.
func (p *proxyRouter) route(username string) (string, error) {
	ip, err := p.router.RouteUser(username)
	if err != nil {
		return "", fmt.Errorf("lmtp/proxy: %w", err)
	}
	return net.JoinHostPort(ip, fmt.Sprint(p.backendPort)), nil
}

// proxyResult is the per-recipient outcome from a proxy delivery.
type proxyResult struct {
	rcpt string
	err  error
}

// proxyForward connects to addr, performs a full LMTP transaction, and returns
// per-recipient results for all rcpts. Runs the entire connection in one call.
func (p *proxyRouter) proxyForward(addr, from string, rcpts []string, data []byte) []proxyResult {
	results := make([]proxyResult, len(rcpts))
	for i, r := range rcpts {
		results[i].rcpt = r
	}

	conn, err := net.DialTimeout("tcp", addr, p.timeout)
	if err != nil {
		connErr := fmt.Errorf("lmtp/proxy: connect %s: %w", addr, err)
		for i := range results {
			results[i].err = connErr
		}
		return results
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(p.timeout)) //nolint:errcheck

	c := goSmtp.NewClientLMTP(conn)
	if err := c.Hello(p.hostname); err != nil {
		helloErr := fmt.Errorf("lmtp/proxy: LHLO %s: %w", addr, err)
		for i := range results {
			results[i].err = helloErr
		}
		return results
	}
	if err := c.Mail(from, nil); err != nil {
		mailErr := fmt.Errorf("lmtp/proxy: MAIL FROM %s: %w", addr, err)
		for i := range results {
			results[i].err = mailErr
		}
		return results
	}

	// Track which recipients passed RCPT TO.
	accepted := make([]int, 0, len(rcpts))
	for i, rcpt := range rcpts {
		if err := c.Rcpt(rcpt, nil); err != nil {
			results[i].err = err
		} else {
			accepted = append(accepted, i)
		}
	}
	if len(accepted) == 0 {
		return results
	}

	wc, err := c.Data()
	if err != nil {
		dataErr := fmt.Errorf("lmtp/proxy: DATA %s: %w", addr, err)
		for _, i := range accepted {
			results[i].err = dataErr
		}
		return results
	}
	if _, err := io.Copy(wc, bytes.NewReader(data)); err != nil {
		writeErr := fmt.Errorf("lmtp/proxy: write %s: %w", addr, err)
		wc.Close() //nolint:errcheck
		for _, i := range accepted {
			results[i].err = writeErr
		}
		return results
	}

	perRcpt, closeErr := wc.CloseWithLMTPResponse()

	rcptIdx := make(map[string]int, len(rcpts))
	for i, r := range rcpts {
		rcptIdx[r] = i
	}
	for rcpt := range perRcpt {
		results[rcptIdx[rcpt]].err = nil
	}
	if lmtpErr, ok := closeErr.(goSmtp.LMTPDataError); ok {
		for rcpt, smtpErr := range lmtpErr {
			if i, found := rcptIdx[rcpt]; found {
				results[i].err = stripRcptPrefix(smtpErr, rcpt)
			}
		}
	} else if closeErr != nil {
		for _, i := range accepted {
			if results[i].err == nil {
				results[i].err = fmt.Errorf("lmtp/proxy: data response %s: %w", addr, closeErr)
			}
		}
	}

	return results
}

// proxyFanOut sends data to all backends in parallel and returns a merged
// per-recipient result map.
func (p *proxyRouter) proxyFanOut(perBackend map[string][]string, from string, data []byte) map[string]error {
	type backendWork struct {
		addr  string
		rcpts []string
	}
	work := make([]backendWork, 0, len(perBackend))
	for addr, rcpts := range perBackend {
		work = append(work, backendWork{addr, rcpts})
	}

	allResults := make(chan []proxyResult, len(work))
	var wg sync.WaitGroup
	for _, w := range work {
		wg.Add(1)
		go func(addr string, rcpts []string) {
			defer wg.Done()
			allResults <- p.proxyForward(addr, from, rcpts, data)
		}(w.addr, w.rcpts)
	}
	wg.Wait()
	close(allResults)

	merged := make(map[string]error)
	for batch := range allResults {
		for _, r := range batch {
			merged[r.rcpt] = r.err
		}
	}
	return merged
}
