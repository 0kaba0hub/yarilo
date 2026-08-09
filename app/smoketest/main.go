// smoketest verifies a live yarilo deployment.
// Usage: smoketest -host mail.example.com -telemetry http://...:8080
//
// Exit 0 = all checks passed.
// Exit 1 = one or more checks failed (prints failures to stderr).
// IMAP conformance is covered by imaptest (see smoke.yml).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	flagHost            = flag.String("host", "localhost", "yarilo hostname")
	flagIMAPHost        = flag.String("imap-host", "", "IMAP hostname for sieve verify step (defaults to -host)")
	flagSMTPHost        = flag.String("smtp-host", "", "SMTP hostname for sieve mail injection (defaults to -host)")
	flagIMAPSPort       = flag.String("imap-port", "993", "IMAPS port (used by sieve verify step)")
	flagPOP3SPort       = flag.String("pop3s-port", "995", "POP3S port")
	flagSMTPMXPort      = flag.String("smtp-mx-port", "25", "SMTP MX port")
	flagSMTPSubPort     = flag.String("smtp-sub-port", "587", "SMTP submission port")
	flagLMTPLoginPort   = flag.String("lmtp-login-port", "24", "yarilo-lmtp-login port")
	flagManageSievePort = flag.String("managesieve-port", "4190", "ManageSieve port")
	flagManageSieveUser = flag.String("managesieve-user", "", "ManageSieve PLAIN auth username")
	flagManageSievePass = flag.String("managesieve-pass", "", "ManageSieve PLAIN auth password")
	flagTelemetry       = flag.String("telemetry", "http://localhost:8080", "telemetry base URL")
	flagTimeout         = flag.Duration("timeout", 10*time.Second, "per-check timeout")
	// FTS catch-up can wait up to fts_timeout (30s) under index lag; the read
	// deadline must exceed that budget or a legitimate wait reads as i/o timeout.
	flagIMAPReadTimeout = flag.Duration("imap-read-timeout", 45*time.Second, "IMAP read deadline for the sieve verify/search steps (must exceed the server fts catch-up budget)")
	flagInsecure        = flag.Bool("insecure", false, "skip TLS certificate verification")
	flagSMTPMX          = flag.Bool("smtp-mx", false, "check SMTP MX EHLO (port -smtp-mx-port)")
	flagSMTPSub         = flag.Bool("smtp-sub", false, "check SMTP submission EHLO+STARTTLS (port -smtp-sub-port)")
	flagProxyProtocol   = flag.Bool("proxy-protocol", false, "send HAProxy PROXY header before SMTP banner")
	flagXClient         = flag.Bool("xclient", false, "check that MX port advertises XCLIENT in EHLO")
	flagPOP3S           = flag.Bool("pop3s", false, "check POP3S greeting and CAPA")
	flagLMTPLogin       = flag.Bool("lmtp-login", false, "check yarilo-lmtp-login LHLO greeting (port -lmtp-login-port)")
	flagManageSieve     = flag.Bool("managesieve", false, "check ManageSieve auth + script CRUD (port -managesieve-port)")
	flagSieve           = flag.Bool("sieve", false, "check Sieve plugin execution via SMTP injection + IMAP verify")
	// The delivery endpoint is named for its role, not for the check that
	// happened to use it first: sieve and FTS both inject a message into a
	// user's mailbox, which is one role (#1202).
	flagDeliveryHost = flag.String("delivery-host", "", "host that accepts the injected mail (defaults to -smtp-host, then -host)")
	flagDeliveryPort = flag.String("delivery-port", "25", "port that accepts the injected mail")
	// Declared, not inferred from the port number: 24/25 is a guess about the
	// topology, and a site running LMTP on 2424 or submission on 587 would get
	// the wrong greeting and an error pointing elsewhere.
	flagDeliveryProto = flag.String("delivery-proto", "smtp", `protocol the delivery endpoint speaks: "smtp" (EHLO) or "lmtp" (LHLO)`)

	flagJMAP           = flag.Bool("jmap", false, "check the JMAP session resource (GET /.well-known/jmap)")
	flagJMAPHost       = flag.String("jmap-host", "", "JMAP hostname (defaults to -host)")
	flagJMAPPort       = flag.String("jmap-port", "8443", "JMAP HTTPS port")
	flagJMAPUser       = flag.String("jmap-user", "", "JMAP Basic auth username")
	flagJMAPPass       = flag.String("jmap-pass", "", "password for -jmap-user")
	flagJMAPMaxRequest = flag.Int("jmap-max-size-request", 10485760,
		"protocol.jmap.jmap_max_size_request of the deployment; the body-cap check sends one byte more")

	flagPasswdFileUser = flag.String("passwd-file-user", "", "IMAP username backed by the passwd-file passdb (enables the check)")
	flagPasswdFilePass = flag.String("passwd-file-pass", "", "password for -passwd-file-user")
	flagStaticUser     = flag.String("static-user", "", "IMAP username backed by the static passdb (enables the check)")
	flagStaticPass     = flag.String("static-pass", "", "password for -static-user")

	flagQuotaUser     = flag.String("quota-user", "", "IMAP username for the QUOTA extension check (enables it)")
	flagQuotaPass     = flag.String("quota-pass", "", "password for -quota-user")
	flagQuotaOverUser = flag.String("quota-over-user", "", "IMAP username provisioned over quota, for the enforcement check (enables it)")
	flagQuotaOverPass = flag.String("quota-over-pass", "", "password for -quota-over-user")
	flagACLUser       = flag.String("acl-user", "", "IMAP username for the ACL extension check (enables it)")
	flagACLPass       = flag.String("acl-pass", "", "password for -acl-user")
	// The shared-namespace disclosure matrix (#1085) needs a second account and
	// a namespace both can name. Enabled only when all three are set.
	flagACLPeerUser     = flag.String("acl-peer-user", "", "second IMAP username for the shared-namespace ACL check")
	flagACLPeerPass     = flag.String("acl-peer-pass", "", "password for -acl-peer-user")
	flagACLSharedPrefix = flag.String("acl-shared-prefix", "", `shared namespace prefix for the ACL check, e.g. "Public/"`)

	flagEnvelopeUser = flag.String("envelope-user", "", "IMAP username whose EXISTING INBOX is probed with FETCH (ENVELOPE BODYSTRUCTURE) (enables it)")
	flagEnvelopePass = flag.String("envelope-pass", "", "password for -envelope-user")

	// A deployment that wants the whole gate says so once, instead of
	// diffing the skipped list by hand on every rollout (#1197).
	flagRequireAll       = flag.Bool("require-all", false, "treat a check disabled by a missing flag as a failure")
	flagRequireAllExcept = flag.String("require-all-except", "",
		"comma-separated check areas -require-all does not demand (e.g. jmap), for deployments that do not run them")

	flagFTSUser = flag.String("fts-user", "", "IMAP username for the full-text search check (enables it)")
	flagFTSPass = flag.String("fts-pass", "", "password for -fts-user")

	flagDirectorAPI      = flag.String("director-api", "", "director admin API base URL, e.g. http://yarilo-director-api:9103 (enables the check, #755)")
	flagDirectorAPIToken = flag.String("director-api-token", "", "director admin API bearer token (defaults to env DIRECTOR_API_TOKEN / YARILO_ADMIN_TOKEN)")
)

// check is one gate item. A non-empty skip means the deployment did not
// configure it: the item stays in the list so the summary describes the
// intended gate, with skip naming the flag that would enable it (#1197).
type check struct {
	area string
	name string
	fn   func() error
	skip string
}

// summary is the reported gate. Returned rather than only logged so the
// counts are assertable.
type summary struct {
	total, passed, failed, skipped int
	// exempt counts the skips forgiven by -require-all-except; they are part
	// of skipped, not a separate bucket.
	exempt int
}

type result struct {
	name string
	err  error
}

func imapHost() string {
	if *flagIMAPHost != "" {
		return *flagIMAPHost
	}
	return *flagHost
}

// deliveryHost resolves the injection endpoint, preferring its own flag and
// falling back to what the checks used before it existed.
func deliveryHost() string {
	if *flagDeliveryHost != "" {
		return *flagDeliveryHost
	}
	return smtpHost()
}

func smtpHost() string {
	if *flagSMTPHost != "" {
		return *flagSMTPHost
	}
	return *flagHost
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	flag.Parse()

	if err := validateDeliveryProto(*flagDeliveryProto); err != nil {
		slog.Error("smoke: bad -delivery-proto", "err", err)
		os.Exit(2)
	}
	checks := register()
	exempt, err := parseExemptions(*flagRequireAllExcept, *flagRequireAll, checks)
	if err != nil {
		slog.Error("smoke: bad -require-all-except", "err", err)
		os.Exit(2)
	}
	if s := runChecks(checks, *flagRequireAll, exempt, os.Stderr); s.failed > 0 {
		os.Exit(1)
	}
}

// validateDeliveryProto refuses a protocol nobody implements rather than
// falling back to EHLO: a typo would otherwise read as "SMTP" and produce the
// LMTP mismatch this flag exists to prevent (#1202).
func validateDeliveryProto(proto string) error {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "smtp", "lmtp":
		return nil
	default:
		return fmt.Errorf("-delivery-proto %q is neither smtp nor lmtp", proto)
	}
}

// register lists the gate. Every check is registered, enabled or not: a
// summary that counts only what ran cannot report what was not asked for, so
// a rollout that loses a flag keeps saying green with fewer checks than last
// time (#1197).
func register() []check {
	var checks []check
	want := func(area string, enabled bool, name, needs string, fn func() error) {
		if enabled {
			checks = append(checks, check{area: area, name: name, fn: fn})
			return
		}
		checks = append(checks, check{area: area, name: name, skip: needs})
	}

	telemetry := *flagTelemetry != ""
	want("telemetry", telemetry, "telemetry /healthz", "needs -telemetry", checkHealth)
	want("telemetry", telemetry, "telemetry /readyz", "needs -telemetry", checkReady)
	want("smtp", *flagSMTPMX, "smtp MX EHLO", "needs -smtp-mx", checkSMTPMX)
	want("smtp", *flagSMTPSub, "smtp submission EHLO+STARTTLS", "needs -smtp-submission", checkSMTPSubmission)
	want("smtp", *flagProxyProtocol, "smtp MX PROXY protocol", "needs -proxy-protocol", checkSMTPProxyProtocol)
	want("smtp", *flagXClient, "smtp MX XCLIENT cap", "needs -xclient", checkSMTPXClient)
	want("pop3s", *flagPOP3S, "pop3s CAPA", "needs -pop3s", checkPOP3S)
	want("lmtp-login", *flagLMTPLogin, "lmtp-login LHLO", "needs -lmtp-login", checkLMTPLogin)
	want("managesieve", *flagManageSieve, "managesieve auth+CRUD", "needs -managesieve", checkManageSieve)
	want("sieve", *flagSieve, "sieve plugins", "needs -sieve", checkSieve)
	want("imap", *flagPasswdFileUser != "", "imap login (passwd-file passdb)", "needs -passwd-file-user", func() error {
		return checkIMAPLogin(*flagPasswdFileUser, *flagPasswdFilePass)
	})
	want("imap", *flagStaticUser != "", "imap login (static passdb)", "needs -static-user", func() error {
		return checkIMAPLogin(*flagStaticUser, *flagStaticPass)
	})
	want("imap", *flagQuotaUser != "", "imap QUOTA (GETQUOTA)", "needs -quota-user", func() error {
		return checkQuota(*flagQuotaUser, *flagQuotaPass)
	})
	want("imap", *flagQuotaOverUser != "", "imap QUOTA enforcement (OVERQUOTA)", "needs -quota-over-user", func() error {
		return checkQuotaOver(*flagQuotaOverUser, *flagQuotaOverPass)
	})
	want("imap", *flagACLUser != "", "imap ACL (MYRIGHTS + SETACL round-trip)", "needs -acl-user", func() error {
		return checkACL(*flagACLUser, *flagACLPass)
	})
	want("imap", *flagACLUser != "" && *flagACLPeerUser != "" && *flagACLSharedPrefix != "", "imap ACL disclosure (shared namespace, peer vs absent mailbox)",
		"needs -acl-user, -acl-peer-user and -acl-shared-prefix", func() error {
			return checkACLDisclosure(*flagACLUser, *flagACLPass,
				*flagACLPeerUser, *flagACLPeerPass, *flagACLSharedPrefix)
		})
	want("imap", *flagEnvelopeUser != "", "imap FETCH (ENVELOPE BODYSTRUCTURE) on an existing INBOX",
		"needs -envelope-user", func() error {
			return checkFetchEnvelope(*flagEnvelopeUser, *flagEnvelopePass)
		})
	want("imap", *flagFTSUser != "", "imap FTS (SEARCH BODY/TEXT/HEADER/FROM)", "needs -fts-user", func() error {
		return checkFTS(*flagFTSUser, *flagFTSPass)
	})
	want("director", *flagDirectorAPI != "", "director admin API status (authenticated)", "needs -director-api", checkDirectorAPI)

	jmap := *flagJMAP
	want("jmap", jmap, "jmap endpoint refuses anonymous access", "needs -jmap", checkJMAPUnauthenticated)
	want("jmap", jmap, "jmap session resource (/.well-known/jmap)", "needs -jmap", checkJMAPSession)
	want("jmap", jmap, "jmap batch + back-reference (Core/echo x2)", "needs -jmap", checkJMAPBatch)
	want("jmap", jmap, "jmap body cap refused at the login edge", "needs -jmap", checkJMAPBodyCap)
	want("jmap", jmap, "jmap Mailbox/get (roles, ids, parent tree)", "needs -jmap", checkJMAPMailboxes)
	want("jmap", jmap, "jmap Mailbox/query + back-referenced get", "needs -jmap", checkJMAPMailboxQuery)
	want("jmap", jmap, "jmap Email/query -> Email/get -> download", "needs -jmap", checkJMAPEmailDiscovery)
	want("jmap", jmap, "jmap download refuses another account's blob", "needs -jmap", checkJMAPDownloadIsolation)
	// Gated on credentials, not on cost: components.jmap ships disabled, so a
	// deployment that never enabled JMAP would fail smoke over a service it
	// does not run.
	want("jmap", jmap && *flagJMAPUser != "" && *flagFTSUser != "",
		"jmap Email/query over full-text search",
		"needs -jmap, -jmap-user and -fts-user (the latter states that FTS is configured)", func() error {
			return checkJMAPFTSQuery(*flagJMAPUser, *flagJMAPPass)
		})
	want("jmap", jmap && *flagJMAPUser != "", "jmap header:* forms, headers, projection and property validation",
		"needs -jmap and -jmap-user", checkJMAPHeaderForms)

	return checks
}

// parseExemptions reads -require-all-except. An area no check declares is an
// error, not a silent no-op: the flag exists to narrow a gate, so a typo in
// it must not read as a narrower gate that quietly still demands everything.
func parseExemptions(list string, requireAll bool, checks []check) (map[string]bool, error) {
	known := map[string]bool{}
	for _, c := range checks {
		known[c.area] = true
	}
	// Alone the flag reads as "demand everything except this" while actually
	// demanding nothing, so it is rejected rather than silently inert.
	if strings.TrimSpace(list) != "" && !requireAll {
		return nil, fmt.Errorf("-require-all-except narrows -require-all, which is not set: nothing is being demanded")
	}
	exempt := map[string]bool{}
	for _, a := range strings.Split(list, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !known[a] {
			areas := slices.Sorted(maps.Keys(known))
			return nil, fmt.Errorf("no smoke check belongs to area %q; known areas: %s", a, strings.Join(areas, ", "))
		}
		exempt[a] = true
	}
	return exempt, nil
}

// runChecks runs the gate and reports it whole: what ran, what failed, and
// what the deployment never asked for.
func runChecks(checks []check, requireAll bool, exempt map[string]bool, out io.Writer) summary {
	slog.Info("smoke: start", "total", len(checks))
	var failures []result
	var skipped, tolerated []check
	passed, exempted := 0, 0
	// An exemption only means something against a demand, so nothing may
	// report itself as exempt when nothing is demanded.
	forgiven := func(area string) bool { return requireAll && exempt[area] }
	for i, c := range checks {
		if c.skip != "" {
			skipped = append(skipped, c)
			// An exemption forgives an area the deployment does not run; it
			// never forgives a check that ran and failed.
			if requireAll && !forgiven(c.area) {
				slog.Error("smoke: SKIP", "n", i+1, "total", len(checks), "check", c.name, "reason", c.skip)
				failures = append(failures, result{c.name, fmt.Errorf("not configured: %s", c.skip)})
				continue
			}
			if forgiven(c.area) {
				exempted++
			}
			tolerated = append(tolerated, c)
			slog.Warn("smoke: SKIP", "n", i+1, "total", len(checks), "check", c.name,
				"reason", c.skip, "area", c.area, "exempt", forgiven(c.area))
			continue
		}
		slog.Info("smoke: run", "n", i+1, "total", len(checks), "check", c.name)
		if err := c.fn(); err != nil {
			slog.Error("smoke: FAIL", "n", i+1, "total", len(checks), "check", c.name, "err", err)
			failures = append(failures, result{c.name, err})
			continue
		}
		passed++
		slog.Info("smoke: OK", "n", i+1, "total", len(checks), "check", c.name)
	}

	// The whole intended gate, not the part that happened to be configured.
	slog.Info("smoke: summary", "checks", len(checks), "passed", passed,
		"failed", len(failures), "skipped", len(skipped), "exempt", exempted)
	// Only the skips not itemised as failures below: listing one twice reads
	// as two distinct problems.
	if len(tolerated) > 0 {
		fmt.Fprintf(out, "\n%d smoke check(s) skipped:\n", len(tolerated))
		for _, c := range tolerated {
			if forgiven(c.area) {
				fmt.Fprintf(out, "  - %s (%s; area %s exempt from -require-all)\n", c.name, c.skip, c.area)
				continue
			}
			fmt.Fprintf(out, "  - %s (%s)\n", c.name, c.skip)
		}
	}
	if len(failures) > 0 {
		fmt.Fprintf(out, "\n%d smoke check(s) failed:\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(out, "  - %s: %v\n", f.name, f.err)
		}
	}
	return summary{total: len(checks), passed: passed, failed: len(failures),
		skipped: len(skipped), exempt: exempted}
}

// ---- IMAP login (passdb drivers) -----------------------------------------

// checkIMAPLogin proves an IMAP LOGIN succeeds for a user served by a specific
// passdb driver (passwd-file / static). It dials IMAPS, authenticates, and
// selects INBOX to confirm the userdb resolved a mailbox for the account.
func checkIMAPLogin(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	return nil
}

// ---- telemetry -----------------------------------------------------------

func checkHealth() error { return httpGet(*flagTelemetry + "/healthz") }
func checkReady() error  { return httpGet(*flagTelemetry + "/readyz") }

func httpGet(url string) error {
	c := &http.Client{Timeout: *flagTimeout}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// checkDirectorAPI verifies the director admin API authenticates a bearer token.
// Hits GET /api/director/ring with the token and asserts 200 with a peer list;
// 403/401 is reported distinctly. Uses /ring because /status is backends-only.
func checkDirectorAPI() error {
	token := *flagDirectorAPIToken
	if token == "" {
		token = os.Getenv("DIRECTOR_API_TOKEN")
	}
	if token == "" {
		token = os.Getenv("YARILO_ADMIN_TOKEN")
	}
	url := strings.TrimRight(*flagDirectorAPI, "/") + "/api/director/ring"
	c := &http.Client{Timeout: *flagTimeout}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A three-member ring already exceeds 512 bytes, and a body cut mid-record
	// cannot be decoded at all (#1203).
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("director API rejected the token (HTTP %d) — the #755 plumbing is broken: %s", resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return checkDirectorStatusBody(body)
}

// directorStatus is the part of the admin API status response this check
// asserts on. The field is "members" -- internal/director/membership.go and
// yarctl both name it that.
type directorStatus struct {
	Self    string `json:"self"`
	Size    int    `json:"size"`
	Members []struct {
		Addr string `json:"addr"`
	} `json:"members"`
}

// checkDirectorStatusBody asserts the ring the response describes, rather than
// looking for a word in it: a substring match passes on any payload that
// happens to contain it, an error one included (#1203).
func checkDirectorStatusBody(body []byte) error {
	var st directorStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return fmt.Errorf("director API status is not decodable: %w: %s", err, body)
	}
	if st.Size < 1 {
		return fmt.Errorf("director API reports a ring of %d members: %s", st.Size, body)
	}
	if len(st.Members) != st.Size {
		return fmt.Errorf("director API reports size %d but lists %d members: %s", st.Size, len(st.Members), body)
	}
	for i, m := range st.Members {
		if m.Addr == "" {
			return fmt.Errorf("director API member %d has no address: %s", i, body)
		}
	}
	return nil
}

// ---- POP3S (port 995) ----------------------------------------------------

func checkPOP3S() error {
	addr := net.JoinHostPort(*flagHost, *flagPOP3SPort)
	dialer := &net.Dialer{Timeout: *flagTimeout}
	tlsCfg := &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	greeting, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if !strings.HasPrefix(greeting, "+OK") {
		return fmt.Errorf("unexpected POP3 greeting: %q", greeting)
	}

	fmt.Fprintf(conn, "CAPA\r\n")
	resp, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("CAPA response: %w", err)
	}
	if !strings.HasPrefix(resp, "+OK") {
		return fmt.Errorf("CAPA failed: %q", resp)
	}
	foundUSER := false
	for {
		line, err := readLine(conn)
		if err != nil {
			return fmt.Errorf("CAPA read: %w", err)
		}
		if line == "." {
			break
		}
		if strings.HasPrefix(line, "USER") {
			foundUSER = true
		}
	}
	if !foundUSER {
		return fmt.Errorf("CAPA missing USER capability")
	}

	fmt.Fprintf(conn, "QUIT\r\n")
	return nil
}

// ---- LMTP login (port 24) ------------------------------------------------

// checkLMTPLogin connects to yarilo-lmtp-login, verifies the 220 banner,
// sends LHLO, and expects a 250 multi-line response with at least one known
// LMTP extension before quitting cleanly.
func checkLMTPLogin() error {
	addr := net.JoinHostPort(*flagHost, *flagLMTPLoginPort)
	dialer := &net.Dialer{Timeout: *flagTimeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	banner, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read banner: %w", err)
	}
	if !strings.HasPrefix(banner, "220") {
		return fmt.Errorf("unexpected banner: %q", banner)
	}

	fmt.Fprintf(conn, "LHLO smoketest\r\n")
	gotOK := false
	for {
		line, err := readLine(conn)
		if err != nil {
			return fmt.Errorf("LHLO read: %w", err)
		}
		if strings.HasPrefix(line, "250-") || strings.HasPrefix(line, "250 ") {
			gotOK = true
			if strings.HasPrefix(line, "250 ") {
				break
			}
			continue
		}
		return fmt.Errorf("LHLO unexpected response: %q", line)
	}
	if !gotOK {
		return fmt.Errorf("LHLO: no 250 response received")
	}

	fmt.Fprintf(conn, "QUIT\r\n")
	return nil
}

// ---- SMTP MX (port 25) ---------------------------------------------------

func checkSMTPMX() error {
	conn, err := smtpDial(net.JoinHostPort(*flagHost, *flagSMTPMXPort), false)
	if err != nil {
		return err
	}
	defer conn.Close()

	caps, err := smtpEHLO(conn)
	if err != nil {
		return err
	}
	_ = caps
	smtpQuit(conn)
	return nil
}

// checkSMTPSubmission verifies the submission port:
//  1. EHLO advertises AUTH PLAIN and STARTTLS.
//  2. Performs the STARTTLS upgrade and sends a second EHLO.
func checkSMTPSubmission() error {
	addr := net.JoinHostPort(*flagHost, *flagSMTPSubPort)
	conn, err := smtpDial(addr, false)
	if err != nil {
		return err
	}
	defer conn.Close()

	caps, err := smtpEHLO(conn)
	if err != nil {
		return err
	}
	if !caps["STARTTLS"] {
		return fmt.Errorf("submission port did not advertise STARTTLS")
	}
	if !caps["AUTH PLAIN"] {
		return fmt.Errorf("submission port did not advertise AUTH PLAIN")
	}

	// Perform STARTTLS upgrade.
	fmt.Fprintf(conn, "STARTTLS\r\n")
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("STARTTLS: read response: %w", err)
	}
	if !strings.HasPrefix(line, "220") {
		return fmt.Errorf("STARTTLS: unexpected response %q", line)
	}
	tlsCfg := &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
	}
	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("STARTTLS TLS handshake: %w", err)
	}

	// Second EHLO after upgrade must still advertise AUTH PLAIN.
	caps2, err := smtpEHLO(tlsConn)
	if err != nil {
		return fmt.Errorf("post-STARTTLS EHLO: %w", err)
	}
	if !caps2["AUTH PLAIN"] {
		return fmt.Errorf("post-STARTTLS EHLO missing AUTH PLAIN")
	}
	smtpQuit(tlsConn)
	return nil
}

// checkSMTPProxyProtocol sends a HAProxy PROXY header before the SMTP banner
// and verifies the server responds with 220.
// Only run when -proxy-protocol flag is set (requires proxy_protocol: true in config).
func checkSMTPProxyProtocol() error {
	addr := net.JoinHostPort(*flagHost, *flagSMTPMXPort)
	dialer := &net.Dialer{Timeout: *flagTimeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	// Send HAProxy PROXY header with a fake source IP.
	fmt.Fprintf(conn, "PROXY TCP4 203.0.113.1 %s 12345 25\r\n", *flagHost)

	// Expect normal SMTP banner.
	banner, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("PROXY: read banner: %w", err)
	}
	if !strings.HasPrefix(banner, "220") {
		return fmt.Errorf("PROXY: unexpected banner %q", banner)
	}
	return nil
}

// checkSMTPXClient connects to MX and verifies EHLO advertises XCLIENT.
// Only run when -xclient flag is set (requires xclient: true in config).
func checkSMTPXClient() error {
	conn, err := smtpDial(net.JoinHostPort(*flagHost, *flagSMTPMXPort), false)
	if err != nil {
		return err
	}
	defer conn.Close()

	caps, err := smtpEHLO(conn)
	if err != nil {
		return err
	}
	if !caps["XCLIENT"] {
		return fmt.Errorf("MX port did not advertise XCLIENT in EHLO")
	}
	smtpQuit(conn)
	return nil
}

// ---- SMTP helpers --------------------------------------------------------

// smtpDial opens a raw TCP connection and reads the 220 banner.
func smtpDial(addr string, _ bool) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: *flagTimeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	banner, err := readLine(conn)
	if err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("read banner: %w", err)
	}
	if !strings.HasPrefix(banner, "220") {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("unexpected banner: %q", banner)
	}
	return conn, nil
}

// smtpEHLO sends EHLO and returns the set of advertised capabilities.
// Keys are normalised: "AUTH PLAIN", "STARTTLS", "XCLIENT", "PIPELINING", …
func smtpEHLO(conn net.Conn) (map[string]bool, error) {
	fmt.Fprintf(conn, "EHLO smoketest\r\n")
	caps := make(map[string]bool)
	for {
		line, err := readLine(conn)
		if err != nil {
			return nil, fmt.Errorf("EHLO read: %w", err)
		}
		// strip "250-" or "250 " prefix
		cap := ""
		if strings.HasPrefix(line, "250-") {
			cap = line[4:]
		} else if strings.HasPrefix(line, "250 ") {
			cap = line[4:]
			caps[cap] = true
			break
		} else {
			return nil, fmt.Errorf("EHLO unexpected: %q", line)
		}
		caps[cap] = true
	}
	return caps, nil
}

func smtpQuit(conn net.Conn) {
	fmt.Fprintf(conn, "QUIT\r\n")
}

// ---- ManageSieve (RFC 5804, port 4190) -----------------------------------

// checkManageSieve connects to the ManageSieve login proxy, authenticates via
// PLAIN, then runs a full script CRUD cycle: LISTSCRIPTS → PUTSCRIPT →
// GETSCRIPT → SETACTIVE → deactivate → DELETESCRIPT → LOGOUT.
func checkManageSieve() error {
	if *flagManageSieveUser == "" {
		return fmt.Errorf("-managesieve-user is required for ManageSieve check")
	}

	conn, err := msieveDial()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err != nil {
		return err
	}

	// LISTSCRIPTS
	fmt.Fprintf(conn, "LISTSCRIPTS\r\n")
	if err := msieveDrainUntilOK(conn); err != nil {
		return fmt.Errorf("LISTSCRIPTS: %w", err)
	}

	// PUTSCRIPT — non-synchronising literal ({N+}) so no continuation needed.
	const scriptName = "smoke-check.sieve"
	const scriptBody = "keep;\n"
	fmt.Fprintf(conn, "PUTSCRIPT %q {%d+}\r\n%s", scriptName, len(scriptBody), scriptBody)
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("PUTSCRIPT response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("PUTSCRIPT failed: %q", line)
	}

	// GETSCRIPT — server responds with {N}\r\n<data>\r\nOK ...
	fmt.Fprintf(conn, "GETSCRIPT %q\r\n", scriptName)
	litHdr, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("GETSCRIPT literal header: %w", err)
	}
	if !strings.HasPrefix(litHdr, "{") {
		return fmt.Errorf("GETSCRIPT unexpected: %q", litHdr)
	}
	sizeStr := strings.TrimSuffix(strings.TrimPrefix(litHdr, "{"), "}")
	size, err := strconv.Atoi(strings.TrimSpace(sizeStr))
	if err != nil {
		return fmt.Errorf("GETSCRIPT literal size %q: %w", litHdr, err)
	}
	got := make([]byte, size)
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("GETSCRIPT body: %w", err)
	}
	if !bytes.Equal(got, []byte(scriptBody)) {
		return fmt.Errorf("GETSCRIPT content mismatch: got %q want %q", got, scriptBody)
	}
	// Drain trailing CRLF + OK line.
	if err := msieveDrainUntilOK(conn); err != nil {
		return fmt.Errorf("GETSCRIPT trailing: %w", err)
	}

	// SETACTIVE — activate the script.
	fmt.Fprintf(conn, "SETACTIVE %q\r\n", scriptName)
	line, err = readLine(conn)
	if err != nil {
		return fmt.Errorf("SETACTIVE response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("SETACTIVE failed: %q", line)
	}

	// SETACTIVE "" — deactivate so we can delete.
	fmt.Fprintf(conn, "SETACTIVE \"\"\r\n")
	line, err = readLine(conn)
	if err != nil {
		return fmt.Errorf("SETACTIVE deactivate response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("SETACTIVE deactivate failed: %q", line)
	}

	// DELETESCRIPT
	fmt.Fprintf(conn, "DELETESCRIPT %q\r\n", scriptName)
	line, err = readLine(conn)
	if err != nil {
		return fmt.Errorf("DELETESCRIPT response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("DELETESCRIPT failed: %q", line)
	}

	// LOGOUT
	fmt.Fprintf(conn, "LOGOUT\r\n")
	return nil
}

// msieveDrainUntilOK reads and discards ManageSieve response lines until a
// line starting with "OK" is found. Returns an error if "NO" or "BYE" is seen.
func msieveDrainUntilOK(conn net.Conn) error {
	for {
		line, err := readLine(conn)
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, "OK") {
			return nil
		}
		if strings.HasPrefix(line, "NO") || strings.HasPrefix(line, "BYE") {
			return fmt.Errorf("server error: %q", line)
		}
	}
}

// ---- low-level -----------------------------------------------------------

func readLine(r io.Reader) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		_, err := r.Read(b)
		if err != nil {
			return "", err
		}
		if b[0] == '\n' {
			break
		}
		buf = append(buf, b[0])
	}
	return strings.TrimRight(string(buf), "\r"), nil
}
