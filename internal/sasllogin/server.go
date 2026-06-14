// Package sasllogin implements a Dovecot auth-client protocol proxy for
// Postfix SASL authentication.
//
// Postfix connects via plain TCP (no mTLS), yarilo-sasl-login dials
// yarilo-auth with mTLS and forwards each session. The Dovecot auth client
// protocol is line-based so line-by-line forwarding is used to enable
// structured logging of auth events and rip= extraction.
//
// Postfix configuration:
//
//	smtpd_sasl_type         = dovecot
//	smtpd_sasl_path         = inet:<host>:12325
//	smtpd_sasl_auth_enable  = yes
package sasllogin

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// Options configures a Server.
type Options struct {
	// AuthAddr is the yarilo-auth client-protocol address (e.g. "yarilo-auth:9100").
	AuthAddr string
	// AuthTLS is the mTLS config for dialing yarilo-auth. nil = plain TCP.
	AuthTLS *tls.Config
	// TrustedNets lists CIDRs allowed to connect. nil = allow all.
	TrustedNets []*net.IPNet
	// HAProxy enables PROXY protocol v1/v2 on the listener.
	HAProxy bool
	// HAProxyTimeout is the read deadline for the PROXY header.
	HAProxyTimeout time.Duration
	// HAProxyNets lists CIDRs whose PROXY header is trusted.
	HAProxyNets []*net.IPNet
}

// Server accepts Postfix connections and proxies each session to yarilo-auth.
type Server struct {
	opts Options
}

// New creates a Server.
func New(opts Options) *Server {
	return &Server{opts: opts}
}

// Serve accepts connections on ln until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.opts.HAProxy {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            haProxyPolicy(s.opts.HAProxyNets),
			ReadHeaderTimeout: timeout,
		}
	}

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			default:
				return fmt.Errorf("sasllogin: accept: %w", err)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(conn)
		}()
	}
}

func (s *Server) handle(client net.Conn) {
	defer client.Close()

	clientIP, _, _ := net.SplitHostPort(client.RemoteAddr().String())

	if !s.isTrusted(client.RemoteAddr()) {
		slog.Warn("sasl-login: rejected untrusted source", "remote_ip", clientIP)
		return
	}

	slog.Info("sasl-login: connect", "remote_ip", clientIP)
	defer slog.Info("sasl-login: disconnect", "remote_ip", clientIP)

	var authConn net.Conn
	var err error
	if s.opts.AuthTLS != nil {
		authConn, err = tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", s.opts.AuthAddr, s.opts.AuthTLS)
	} else {
		authConn, err = net.DialTimeout("tcp", s.opts.AuthAddr, 5*time.Second)
	}
	if err != nil {
		slog.Error("sasl-login: dial auth failed", "remote_ip", clientIP, "err", err)
		return
	}
	defer authConn.Close()

	clientRd := bufio.NewReader(client)
	authRd := bufio.NewReader(authConn)

	// auth → client: forward server lines, log OK/FAIL results.
	authDone := make(chan struct{})
	go func() {
		defer close(authDone)
		for {
			line, err := authRd.ReadString('\n')
			if err != nil {
				return
			}
			logServerLine(line, clientIP)
			if _, werr := client.Write([]byte(line)); werr != nil {
				return
			}
		}
	}()

	// client → auth: forward client lines, log AUTH requests.
	for {
		line, err := clientRd.ReadString('\n')
		if err != nil {
			return
		}
		logClientLine(line, clientIP)
		if _, werr := authConn.Write([]byte(line)); werr != nil {
			return
		}
	}
}

// isTrusted returns true when TrustedNets is empty (allow all) or when addr
// falls within one of the configured nets.
func (s *Server) isTrusted(addr net.Addr) bool {
	if len(s.opts.TrustedNets) == 0 {
		return true
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, n := range s.opts.TrustedNets {
		if n.Contains(tcpAddr.IP) {
			return true
		}
	}
	return false
}

// logClientLine extracts AUTH parameters for structured logging.
// Only AUTH and CONT lines are logged at Info; everything else at Debug.
func logClientLine(line, remoteIP string) {
	trimmed := strings.TrimRight(line, "\r\n")
	parts := strings.Split(trimmed, "\t")
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "AUTH":
		attrs := map[string]string{}
		for _, p := range parts[3:] {
			if k, v, ok := strings.Cut(p, "="); ok {
				attrs[k] = v
			}
		}
		mech := ""
		if len(parts) > 2 {
			mech = parts[2]
		}
		slog.Info("sasl-login: auth request",
			"remote_ip", remoteIP,
			"mech", mech,
			"service", attrs["service"],
			"rip", attrs["rip"],
			"lip", attrs["lip"],
		)
	case "CONT":
		slog.Debug("sasl-login: auth continue", "remote_ip", remoteIP)
	case "CANCEL":
		slog.Info("sasl-login: auth cancel", "remote_ip", remoteIP)
	default:
		slog.Debug("sasl-login: client line", "remote_ip", remoteIP, "cmd", parts[0])
	}
}

// logServerLine extracts OK/FAIL outcomes for structured logging.
func logServerLine(line, remoteIP string) {
	trimmed := strings.TrimRight(line, "\r\n")
	parts := strings.Split(trimmed, "\t")
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "OK":
		attrs := map[string]string{}
		for _, p := range parts[2:] {
			if k, v, ok := strings.Cut(p, "="); ok {
				attrs[k] = v
			}
		}
		slog.Info("sasl-login: auth ok", "remote_ip", remoteIP, "user", attrs["user"])
	case "FAIL":
		attrs := map[string]string{}
		for _, p := range parts[2:] {
			if k, v, ok := strings.Cut(p, "="); ok {
				attrs[k] = v
			}
		}
		slog.Info("sasl-login: auth fail",
			"remote_ip", remoteIP,
			"user", attrs["user"],
			"reason", attrs["reason"],
			"code", attrs["code"],
		)
	default:
		slog.Debug("sasl-login: server line", "remote_ip", remoteIP, "cmd", parts[0])
	}
}

func haProxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcpAddr, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcpAddr.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}
