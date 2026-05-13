// Package dmarc implements DMARC policy evaluation (RFC 7489).
// It fetches the _dmarc TXT record, parses the policy, and evaluates
// SPF + DKIM alignment against the RFC5322.From domain.
package dmarc

import (
	"context"
	"fmt"
	"net"
	"strings"

	goSpf "blitiri.com.ar/go/spf"
	"github.com/0kaba0hub/yarilo/internal/dkim"
)

// Policy is the DMARC disposition policy.
type Policy string

const (
	PolicyNone       Policy = "none"
	PolicyQuarantine Policy = "quarantine"
	PolicyReject     Policy = "reject"
)

// Alignment is the DKIM/SPF identifier alignment mode.
type Alignment string

const (
	AlignmentRelaxed Alignment = "r"
	AlignmentStrict  Alignment = "s"
)

// Record holds the parsed DMARC record fields relevant to policy evaluation.
type Record struct {
	Policy   Policy
	SPPolicy Policy    // subdomain policy (sp=); defaults to Policy if absent
	ADKIM    Alignment // DKIM alignment; default relaxed
	ASPF     Alignment // SPF alignment; default relaxed
}

// Result is the outcome of a DMARC evaluation.
type Result struct {
	Disposition Policy
	DKIMPass    bool
	SPFPass     bool
	Err         error
}

// Evaluate performs DMARC policy evaluation.
// fromDomain is the RFC5322.From header domain.
// spfResult and spfDomain are from the envelope SPF check.
// dkimResults are from DKIM verification.
func Evaluate(ctx context.Context, fromDomain string, spfResult goSpf.Result, spfDomain string, dkimResults []dkim.Result) Result {
	rec, err := fetchRecord(ctx, fromDomain)
	if err != nil {
		// No DMARC record → treat as p=none.
		return Result{Disposition: PolicyNone, Err: err}
	}

	orgFrom := orgDomain(fromDomain)
	dkimPass := checkDKIMAlignment(dkimResults, orgFrom, rec.ADKIM)
	spfPass := checkSPFAlignment(spfResult, spfDomain, orgFrom, rec.ASPF)

	var disposition Policy
	if dkimPass || spfPass {
		disposition = PolicyNone
	} else {
		disposition = rec.Policy
	}

	return Result{
		Disposition: disposition,
		DKIMPass:    dkimPass,
		SPFPass:     spfPass,
	}
}

// fetchRecord looks up and parses the DMARC TXT record for domain.
func fetchRecord(ctx context.Context, domain string) (Record, error) {
	r := &net.Resolver{}
	txts, err := r.LookupTXT(ctx, "_dmarc."+domain)
	if err != nil {
		return Record{}, fmt.Errorf("dmarc/dns: %w", err)
	}
	for _, txt := range txts {
		if strings.HasPrefix(txt, "v=DMARC1") {
			return parseRecord(txt)
		}
	}
	return Record{}, fmt.Errorf("dmarc: no DMARC record for %q", domain)
}

func parseRecord(txt string) (Record, error) {
	rec := Record{
		Policy: PolicyNone,
		ADKIM:  AlignmentRelaxed,
		ASPF:   AlignmentRelaxed,
	}
	for _, tag := range strings.Split(txt, ";") {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		eq := strings.IndexByte(tag, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(tag[:eq])
		v := strings.TrimSpace(tag[eq+1:])
		switch k {
		case "p":
			rec.Policy = parsePolicy(v)
		case "sp":
			rec.SPPolicy = parsePolicy(v)
		case "adkim":
			rec.ADKIM = parseAlignment(v)
		case "aspf":
			rec.ASPF = parseAlignment(v)
		}
	}
	if rec.SPPolicy == "" {
		rec.SPPolicy = rec.Policy
	}
	return rec, nil
}

func parsePolicy(v string) Policy {
	switch strings.ToLower(v) {
	case "quarantine":
		return PolicyQuarantine
	case "reject":
		return PolicyReject
	default:
		return PolicyNone
	}
}

func parseAlignment(v string) Alignment {
	if strings.ToLower(v) == "s" {
		return AlignmentStrict
	}
	return AlignmentRelaxed
}

// checkDKIMAlignment returns true if any DKIM result passes and aligns with fromOrgDomain.
func checkDKIMAlignment(results []dkim.Result, fromOrgDomain string, mode Alignment) bool {
	for _, r := range results {
		if !r.Pass {
			continue
		}
		d := orgDomain(r.Domain)
		if mode == AlignmentStrict {
			if strings.EqualFold(r.Domain, fromOrgDomain) {
				return true
			}
		} else {
			if strings.EqualFold(d, fromOrgDomain) {
				return true
			}
		}
	}
	return false
}

// checkSPFAlignment returns true if SPF passed and the envelope domain aligns.
func checkSPFAlignment(result goSpf.Result, spfDomain, fromOrgDomain string, mode Alignment) bool {
	if result != goSpf.Pass {
		return false
	}
	d := orgDomain(spfDomain)
	if mode == AlignmentStrict {
		return strings.EqualFold(spfDomain, fromOrgDomain)
	}
	return strings.EqualFold(d, fromOrgDomain)
}

// orgDomain returns the organisational domain: last two labels of a FQDN.
// e.g. mail.example.com → example.com
func orgDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	parts := strings.Split(domain, ".")
	if len(parts) <= 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
