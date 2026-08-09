package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// fakeDeliveryServer answers like the listener it is told to be: an LMTP
// server refuses EHLO the way yarilo-lmtp-login does, an SMTP one refuses
// LHLO. Greeting the wrong one is then a failure the test can observe rather
// than a difference in wording.
func fakeDeliveryServer(t *testing.T, lmtp bool) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				r := bufio.NewReader(conn)
				fmt.Fprintf(conn, "220 fake ready\r\n") //nolint:errcheck
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.Fields(strings.TrimSpace(line) + " ")[0])
					switch {
					case cmd == "LHLO" && !lmtp:
						fmt.Fprintf(conn, "500 5.5.1 Error: command not recognized\r\n") //nolint:errcheck
					case cmd == "EHLO" && lmtp:
						fmt.Fprintf(conn, "500 5.5.1 This is a LMTP server, use LHLO\r\n") //nolint:errcheck
					case cmd == "LHLO" || cmd == "EHLO":
						fmt.Fprintf(conn, "250-fake\r\n250 PIPELINING\r\n") //nolint:errcheck
					case cmd == "DATA":
						fmt.Fprintf(conn, "354 go ahead\r\n") //nolint:errcheck
						for {
							l, err := r.ReadString('\n')
							if err != nil {
								return
							}
							if strings.TrimRight(l, "\r\n") == "." {
								break
							}
						}
						fmt.Fprintf(conn, "250 2.0.0 accepted\r\n") //nolint:errcheck
					case cmd == "QUIT":
						fmt.Fprintf(conn, "221 bye\r\n") //nolint:errcheck
						return
					default:
						fmt.Fprintf(conn, "250 ok\r\n") //nolint:errcheck
					}
				}
			}()
		}
	}()

	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return host, port
}

// pointDeliveryAt makes the injector talk to the fake, and restores the flags.
func pointDeliveryAt(t *testing.T, host, port string, lmtp bool) {
	t.Helper()
	proto := "smtp"
	if lmtp {
		proto = "lmtp"
	}
	oldHost, oldPort, oldProto := *flagDeliveryHost, *flagDeliveryPort, *flagDeliveryProto
	*flagDeliveryHost, *flagDeliveryPort, *flagDeliveryProto = host, port, proto
	t.Cleanup(func() {
		*flagDeliveryHost, *flagDeliveryPort, *flagDeliveryProto = oldHost, oldPort, oldProto
	})
}

// The sieve and FTS checks deliver through this helper, so greeting an LMTP
// listener with EHLO made both unrunnable against yarilo-lmtp-login -- which
// is the topology the chart ships (#1202).
func TestDeliveryGreetingMatchesTheListener(t *testing.T) {
	cases := []struct {
		name       string
		serverLMTP bool
		flagLMTP   bool
		wantErr    bool
	}{
		{"LMTP listener, declared", true, true, false},
		{"LMTP listener, not declared", true, false, true},
		{"SMTP listener, not declared", false, false, false},
		// The flag is a claim about the deployment, so a wrong claim must fail
		// rather than be quietly corrected.
		{"SMTP listener, wrongly declared LMTP", false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := fakeDeliveryServer(t, tc.serverLMTP)
			pointDeliveryAt(t, host, port, tc.flagLMTP)

			err := lmtpSendRaw("smoke@example.com", "u1@example.com",
				"Subject: probe\r\n\r\nbody\r\n")
			if tc.wantErr && err == nil {
				t.Error("delivery succeeded against a listener that speaks the other protocol")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("delivery failed: %v", err)
			}
		})
	}
}

func TestDeliveryGreetingIsDeclaredNotGuessed(t *testing.T) {
	old := *flagDeliveryProto
	t.Cleanup(func() { *flagDeliveryProto = old })

	for proto, want := range map[string]string{
		"lmtp": "LHLO smoketest",
		"LMTP": "LHLO smoketest", // the value is a name, not a token to match exactly
		"smtp": "EHLO smoketest",
	} {
		*flagDeliveryProto = proto
		if got := deliveryGreeting(); got != want {
			t.Errorf("-delivery-proto %q greets with %q, want %q", proto, got, want)
		}
	}
}

// A protocol nobody implements must stop the run: falling back to EHLO would
// read as "smtp" and reproduce the mismatch the flag exists to prevent.
func TestDeliveryProtoIsValidated(t *testing.T) {
	for _, proto := range []string{"smtp", "lmtp", " LMTP "} {
		if err := validateDeliveryProto(proto); err != nil {
			t.Errorf("validateDeliveryProto(%q) = %v, want accepted", proto, err)
		}
	}
	for _, proto := range []string{"lmpt", "esmtp", "", "smtp lmtp"} {
		if err := validateDeliveryProto(proto); err == nil {
			t.Errorf("validateDeliveryProto(%q) accepted a protocol nothing speaks", proto)
		}
	}
}

// The delivery endpoint has its own host, and falls back to what the checks
// used before it existed rather than changing an operator's run.
func TestDeliveryHostPrefersItsOwnFlag(t *testing.T) {
	oldDelivery, oldSMTP, oldHost := *flagDeliveryHost, *flagSMTPHost, *flagHost
	t.Cleanup(func() { *flagDeliveryHost, *flagSMTPHost, *flagHost = oldDelivery, oldSMTP, oldHost })

	*flagHost, *flagSMTPHost, *flagDeliveryHost = "plain", "smtp", "delivery"
	if got := deliveryHost(); got != "delivery" {
		t.Errorf("deliveryHost() = %q, want its own flag", got)
	}
	*flagDeliveryHost = ""
	if got := deliveryHost(); got != "smtp" {
		t.Errorf("deliveryHost() = %q, want the smtp fallback", got)
	}
	*flagSMTPHost = ""
	if got := deliveryHost(); got != "plain" {
		t.Errorf("deliveryHost() = %q, want -host", got)
	}
}

// The gate asserts the ring the status describes. A word-match on a truncated
// body passed on payloads that describe no ring at all, and missed the real
// one because the field is "members", not "peers" (#1203).
func TestDirectorStatusBodyAssertsTheRing(t *testing.T) {
	healthy := `{"schemaVersion":1,"self":"10.1.114.23:9102","size":3,"members":[
		{"addr":"10.1.114.8:9102","index":0},{"addr":"10.1.114.23:9102","index":1},
		{"addr":"10.1.114.9:9102","index":2}]}`

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"a healthy three-member ring", healthy, false},
		{"a single-node ring", `{"size":1,"members":[{"addr":"10.0.0.1:9102"}]}`, false},
		// What the old check looked for: nothing in the tree emits it.
		{"the peers spelling", `{"size":3,"peers":[{"addr":"10.0.0.1:9102"}]}`, true},
		// What the 512-byte limit produced on a three-member ring.
		{"a body cut mid-record", healthy[:len(healthy)/2], true},
		{"an empty ring", `{"size":0,"members":[]}`, true},
		{"size disagreeing with the list", `{"size":3,"members":[{"addr":"10.0.0.1:9102"}]}`, true},
		{"a member with no address", `{"size":1,"members":[{"index":0}]}`, true},
		// A substring match passed on this: it contains the word and no ring.
		{"an error payload naming members", `{"error":"members unavailable"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDirectorStatusBody([]byte(tc.body))
			if tc.wantErr && err == nil {
				t.Errorf("accepted a body that describes no healthy ring: %s", tc.body)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("rejected a healthy ring: %v", err)
			}
		})
	}
}
