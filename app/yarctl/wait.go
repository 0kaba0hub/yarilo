package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// dispatchWait implements `yarctl wait` — a dependency probe for use as a
// Kubernetes startupProbe, replacing the wait-* init containers.
//
// Why this instead of busybox wget in an exec probe:
//
//   - exit codes are ours. busybox wget returns 1 for DNS failure, connection
//     refused, an HTTP error status and a timeout alike, so the probe's output in
//     `kubectl describe` says nothing about which of them happened.
//   - the timeout covers the whole attempt. busybox -T bounds reads only, and DNS
//     resolution can hang outside it; here one context deadline covers dial,
//     resolve, request and response.
//   - it needs no extra image. The wait-* containers pull busybox separately;
//     yarctl already ships in the runtime image, so the probe uses the same one as
//     the application.
//   - it can grow mTLS if a future probe targets a protocol port rather than the
//     plain-HTTP telemetry port, which wget cannot do at all.
//
// Two target schemes are supported:
//
//   - http(s)://…  — a GET that must answer 2xx (a telemetry /readyz endpoint).
//   - tcp://host:port — a TCP connect, for a dependency with no HTTP endpoint
//     (a database or Redis), replacing the wait-* init containers' TCP wait.
//
// Usage:
//
//	yarctl wait [--timeout 2s] http://yarilo-auth:8080/readyz tcp://db:5432 tcp://redis:6379
//
// URLs are POSITIONAL, not a --url flag: yarctl already registers a global --url
// (the Director API base URL), and global flags are extracted from argv before a
// subcommand ever sees them, so a --url here would be swallowed and the probe
// would fail with "at least one URL is required" no matter what was passed.
//
// Exit 0 when every target is reachable (2xx for http, an open connection for tcp).
// Non-zero otherwise, with the failing target and the reason on stderr.
func dispatchWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 2*time.Second, "deadline for each probe, covering DNS, dial and response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	urls := fs.Args()
	if len(urls) == 0 {
		return errors.New("wait: at least one URL argument is required (usage: yarctl wait [--timeout 2s] URL...)")
	}

	for _, u := range urls {
		if err := probeURL(u, *timeout); err != nil {
			// stderr, not the error return: kubelet shows the probe's output, and a
			// bare message reads better there than a wrapped CLI error.
			fmt.Fprintf(os.Stderr, "not ready: %v\n", err)
			os.Exit(1)
		}
	}
	return nil
}

// probeURL reports nil when u is reachable within timeout. It dispatches by scheme:
// tcp://host:port opens a TCP connection (for dependencies with no HTTP endpoint,
// e.g. a database or Redis); http(s)://… does a GET and checks for a 2xx status.
func probeURL(u string, timeout time.Duration) error {
	if addr, ok := strings.CutPrefix(u, "tcp://"); ok {
		return probeTCP(addr, timeout)
	}
	return probeHTTP(u, timeout)
}

// probeTCP reports nil when a TCP connection to addr (host:port) can be established
// within timeout. The timeout covers DNS resolution and the dial together.
func probeTCP(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("tcp://%s: %w", addr, err)
	}
	_ = conn.Close()
	return nil
}

// probeHTTP reports nil when u answers 2xx within timeout.
func probeHTTP(u string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	// A dedicated client, not http.DefaultClient: the probe must not inherit any
	// global transport state, and keep-alive is pointless for a one-shot process.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return nil
}
