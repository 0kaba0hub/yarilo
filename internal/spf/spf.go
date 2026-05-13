// Package spf wraps blitiri.com.ar/go/spf for envelope sender verification.
package spf

import (
	"context"
	"fmt"
	"net"
	"strings"

	goSpf "blitiri.com.ar/go/spf"
)

// Result re-exports the blitiri SPF result type.
type Result = goSpf.Result

// Re-exported result values.
var (
	None      = goSpf.None
	Neutral   = goSpf.Neutral
	Pass      = goSpf.Pass
	Fail      = goSpf.Fail
	SoftFail  = goSpf.SoftFail
	TempError = goSpf.TempError
	PermError = goSpf.PermError
)

// Check verifies the SPF record for the envelope sender.
// mailFrom is used when non-empty; otherwise ehloFrom is used.
// Returns the SPF result and any DNS-level error.
func Check(_ context.Context, ip net.IP, mailFrom, ehloFrom string) (Result, error) {
	sender := mailFrom
	if sender == "" {
		sender = ehloFrom
	}

	domain, err := extractDomain(sender)
	if err != nil {
		return PermError, fmt.Errorf("spf/check: %w", err)
	}

	result, err := goSpf.CheckHostWithSender(ip, ehloFrom, sender)
	if err != nil {
		return result, fmt.Errorf("spf/check %s: %w", domain, err)
	}
	return result, nil
}

func extractDomain(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	addr = strings.Trim(addr, "<>")
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		if addr == "" {
			return "", nil // null sender (<>) is valid for bounces
		}
		return "", fmt.Errorf("no @ in address %q", addr)
	}
	return addr[at+1:], nil
}
