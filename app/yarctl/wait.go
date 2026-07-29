package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
// Usage:
//
//	yarctl wait --url http://yarilo-auth:8080/readyz \
//	            --url http://yarilo-anvil:8080/readyz [--timeout 2s]
//
// Exit 0 when every URL answers 2xx. Non-zero otherwise, with the failing URL and
// the reason on stderr.
func dispatchWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	var urls stringList
	fs.Var(&urls, "url", "URL to probe; repeat for several dependencies (all must pass)")
	timeout := fs.Duration("timeout", 2*time.Second, "deadline for each probe, covering DNS, dial and response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(urls) == 0 {
		return errors.New("wait: at least one --url is required")
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

// probeURL reports nil when u answers 2xx within timeout.
func probeURL(u string, timeout time.Duration) error {
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

// stringList collects a repeated flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	if v == "" {
		return errors.New("empty URL")
	}
	*l = append(*l, v)
	return nil
}
