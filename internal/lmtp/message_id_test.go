package lmtp

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/config"
)

// A message stored without a Message-ID can never be replied to or threaded:
// it is its own root in the conversation sidecar for ever, no later reply can
// name it, and JMAP reports a null id. The header is part of the stored bytes,
// so it cannot be added afterwards without rewriting mail.
//
// Asserted on prependHeaders, which is the function that decides, and which
// runs before Sieve, before storage and before the thread sidecar -- so what it
// returns is what all three read.
func TestAMessageWithoutAMessageIDGetsOne(t *testing.T) {
	msgID := regexp.MustCompile(`(?mi)^Message-ID: <[0-9a-f]{32}@[^>]+>\r?$`)

	tests := []struct {
		name    string
		in      string
		enabled bool
		// want is what must be true of the result: either the bytes are
		// untouched, or exactly one Message-ID appears.
		unchanged bool
		wantID    bool
	}{
		{
			name:    "no Message-ID at all",
			in:      "From: a@x\r\nTo: b@y\r\nSubject: hi\r\n\r\nbody\r\n",
			enabled: true, wantID: true,
		},
		{
			name:    "already has one, in the canonical spelling",
			in:      "From: a@x\r\nMessage-ID: <keep-me@x>\r\nTo: b@y\r\n\r\nbody\r\n",
			enabled: true, unchanged: true,
		},
		{
			name:    "already has one, lower case",
			in:      "From: a@x\r\nmessage-id: <keep-me@x>\r\n\r\nbody\r\n",
			enabled: true, unchanged: true,
		},
		{
			name:    "already has one, mixed case",
			in:      "From: a@x\r\nMeSsAgE-iD: <keep-me@x>\r\n\r\nbody\r\n",
			enabled: true, unchanged: true,
		},
		{
			name: "malformed one is still not rewritten",
			in:   "From: a@x\r\nMessage-ID: not-an-addr-spec\r\n\r\nbody\r\n",
			// Whatever a sender wrote is what a reply quotes back in
			// References; rewriting it would break the thread it belongs to.
			enabled: true, unchanged: true,
		},
		{
			name: "one inside a forwarded message does not count",
			in: "From: a@x\r\nTo: b@y\r\nSubject: Fwd: hi\r\n\r\n" +
				"---------- Forwarded message ----------\r\n" +
				"From: c@z\r\nMessage-ID: <inner@z>\r\nSubject: hi\r\n\r\ninner body\r\n",
			// At column zero and correctly spelled, so a scan that does not
			// stop at the blank line finds it and calls this message
			// identified -- which leaves the forward itself unnameable, and a
			// forward is precisely the mail whose body carries other headers.
			enabled: true, wantID: true,
		},
		{
			name:    "the last header before the blank line",
			in:      "From: a@x\r\nMessage-ID: <keep-me@x>\r\n\r\nbody\r\n",
			enabled: true, unchanged: true,
		},
		{
			name:    "a continuation line is not a field",
			in:      "From: a@x\r\nSubject: long\r\n Message-ID: <not-a-field@x>\r\n\r\nbody\r\n",
			enabled: true, wantID: true,
		},
		{
			name:    "bare LF line endings",
			in:      "From: a@x\nMessage-ID: <keep-me@x>\n\nbody\n",
			enabled: true, unchanged: true,
		},
		{
			name:    "switched off, nothing is added",
			in:      "From: a@x\r\nTo: b@y\r\n\r\nbody\r\n",
			enabled: false, unchanged: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{opts: Options{
				Hostname: "mx.example.test",
				Config: config.LMTPProtocolConfig{
					AddMessageID:       tt.enabled,
					HdrDeliveryAddress: "none",
				},
			}}
			got := string(s.prependHeaders([]byte(tt.in), "b@y", "b@y"))

			if tt.unchanged {
				if got != tt.in {
					t.Errorf("message was rewritten:\n got: %q\nwant: %q", got, tt.in)
				}
				return
			}
			// Counted in the header section alone: a forwarded message carries
			// the headers of the message it forwards, and those are body.
			if n := countMessageIDFields(got); n != 1 {
				t.Errorf("header section carries %d Message-ID fields, want exactly 1:\n%s", n, got)
			}
			if tt.wantID && !msgID.MatchString(got) {
				t.Errorf("no synthesised Message-ID of the expected shape:\n%s", got)
			}
			if !strings.HasSuffix(got, tt.in) {
				t.Error("the original message is no longer intact underneath the added headers")
			}
		})
	}
}

// countMessageIDFields counts the Message-ID fields of the header section,
// stopping at the blank line for the same reason the code does.
func countMessageIDFields(msg string) int {
	n := 0
	for _, line := range strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "message-id:") {
			n++
		}
	}
	return n
}

// Two deliveries must not share an identifier, or the thing being handed out is
// a constant rather than an identity.
func TestSynthesisedMessageIDsAreUnique(t *testing.T) {
	s := &session{opts: Options{
		Hostname: "mx.example.test",
		Config:   config.LMTPProtocolConfig{AddMessageID: true, HdrDeliveryAddress: "none"},
	}}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		out := s.prependHeaders([]byte("From: a@x\r\n\r\nbody\r\n"), "b@y", "b@y")
		id := string(bytes.SplitN(out, []byte("\r\n"), 2)[0])
		if seen[id] {
			t.Fatalf("%s was handed out twice", id)
		}
		seen[id] = true
	}
}

// The synthesised header must be in the bytes every consumer downstream reads.
//
// This is the part that cannot be argued from the source: prependHeaders runs
// before Sieve, before storage and before the thread sidecar, so a reply naming
// the synthesised id must land in the same conversation as the message that was
// given it. If the header were added anywhere later -- at storage, say -- the
// stored message would carry an identity the conversation never saw, and the
// reply would start a thread of its own while everything still looked correct.
func TestASynthesisedMessageIDThreadsAReply(t *testing.T) {
	s, box, ui, info := threadingSession(t)
	s.opts.Hostname = "mx.example.test"
	s.opts.Config = config.LMTPProtocolConfig{AddMessageID: true, HdrDeliveryAddress: "none"}

	// Arrives with no identity of its own.
	first := s.prependHeaders([]byte("Subject: Plan\r\nFrom: a@x\r\n\r\nbody\r\n"), "alice@x", "alice@x")
	id := messageIDOf(t, first)
	if id == "" {
		t.Fatal("delivery was not given a Message-ID, so nothing can ever reply to it")
	}
	firstGUID := deliverThreaded(t, s, box, ui, info, string(first))

	// A reply naming it, the way a client would.
	reply := s.prependHeaders([]byte("Subject: Re: Plan\r\nFrom: b@y\r\nIn-Reply-To: "+id+"\r\nReferences: "+id+"\r\n\r\nreply\r\n"),
		"alice@x", "alice@x")
	replyGUID := deliverThreaded(t, s, box, ui, info, string(reply))

	state, err := threads.Load(threads.PathFor(info))
	if err != nil {
		t.Fatalf("load sidecar: %v", err)
	}
	firstThread, ok := state.ThreadOfGUID(firstGUID)
	if !ok {
		t.Fatal("the first message is not in the sidecar")
	}
	replyThread, ok := state.ThreadOfGUID(replyGUID)
	if !ok {
		t.Fatal("the reply is not in the sidecar")
	}
	if firstThread != replyThread {
		t.Errorf("the reply started its own conversation (%s) instead of joining %s; the id it names was never seen by the sidecar",
			replyThread, firstThread)
	}
}

// messageIDOf returns the value of the Message-ID field, or "" when there is
// none.
func messageIDOf(t *testing.T, raw []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(raw), "\r\n") {
		if line == "" {
			return ""
		}
		if strings.HasPrefix(strings.ToLower(line), "message-id:") {
			return strings.TrimSpace(line[len("message-id:"):])
		}
	}
	return ""
}

// A Sieve script must see the synthesised header.
//
// The other half of "before Sieve, before storage, before the sidecar": the
// threading test proves the sidecar sees it, and this proves the script does.
// Both halves matter because a header added between them would satisfy one and
// not the other, and neither the delivered mail nor the log would say so.
//
// The script tests exactly the fact: a message with no id of its own is filed
// by a rule that matches on message-id.
func TestASynthesisedMessageIDIsVisibleToSieve(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	engine := sieve.New(config.SieveConfig{
		Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536,
		DefaultName: sieve.FallbackDefaultName,
	}, nil, nil, nil)
	store := &sieve.FsScriptStore{DefaultName: sieve.FallbackDefaultName}
	script := []byte(`require "fileinto";
if header :contains "message-id" "@" { fileinto "seen"; }`)
	if err := store.SaveScript(ctx, "u1", home, "test", script); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", home, "test"); err != nil {
		t.Fatal(err)
	}

	s := &session{opts: Options{
		Hostname: "mx.example.test",
		Config:   config.LMTPProtocolConfig{AddMessageID: true, HdrDeliveryAddress: "none"},
	}}
	// No Message-ID of its own: without the synthesis the rule cannot match.
	msg := s.prependHeaders([]byte("From: a@x\r\nTo: u1@example.com\r\nSubject: hi\r\n\r\nbody\r\n"), "u1@example.com", "u1@example.com")

	result, err := engine.Filter(ctx, sieve.FilterOptions{
		Username: "u1", HomeDir: home,
		EnvFrom: "a@x", EnvTo: "u1@example.com",
		MsgRaw: msg,
	})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if result == nil || len(result.Deliveries) != 1 {
		t.Fatalf("expected one delivery, got %+v", result)
	}
	if got := result.Deliveries[0].Folder; got != "seen" {
		t.Errorf("filed into %q, want seen; the script did not see a message-id, so the header reaches storage without ever reaching Sieve", got)
	}
}

// One name in all three places delivery writes: the Message-ID, the Received
// header and the LHLO banner take it from the same field, so a deployment
// cannot answer with one name and stamp another.
//
// The banner is asserted through the same option the server reads, since
// building a server here would test the wiring of a test.
func TestOneHostnameNamesEveryHeaderDeliveryWrites(t *testing.T) {
	const host = "mx.example.test"
	s := &session{
		from: "sender@elsewhere.invalid",
		opts: Options{
			Hostname: host,
			Config: config.LMTPProtocolConfig{
				AddMessageID: true, AddReceivedHeader: true, HdrDeliveryAddress: "none",
			},
		},
	}
	out := string(s.prependHeaders([]byte("From: a@x\r\nTo: b@y\r\n\r\nbody\r\n"), "b@y", "b@y"))

	id := messageIDOf(t, []byte(out))
	if !strings.HasSuffix(id, "@"+host+">") {
		t.Errorf("Message-ID is %q, want it at %s", id, host)
	}
	if !strings.Contains(out, "by "+host+" with LMTP") {
		t.Errorf("Received does not name %s:\n%s", host, out)
	}
	// The literal is what both used to carry, so it must not be anywhere.
	if strings.Contains(out, "by yarilo with LMTP") || strings.Contains(id, "@yarilo>") {
		t.Errorf("the literal fallback is still in the headers:\n%s", out)
	}
}
