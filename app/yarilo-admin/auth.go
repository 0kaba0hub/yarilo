package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/pkg/authclient"
)

var (
	authAddr string
	authCert string
	authKey  string
	authCA   string
)

func dispatchAuth(args []string) error {
	if len(args) == 0 {
		printAuthUsage()
		return nil
	}
	switch args[0] {
	case "cache":
		return dispatchAuthCache(args[1:])
	case "scram-verifier":
		return cmdAuthSCRAMVerifier(args[1:])
	default:
		return fmt.Errorf("unknown auth command %q — available: cache, scram-verifier", args[0])
	}
}

// cmdAuthSCRAMVerifier reads a plain password (from --password or
// stdin) and prints a {SCRAM-SHA-256} verifier blob suitable for
// dropping into the SQL `password` column. The plain password
// never crosses any network boundary — derivation runs locally.
//
// Usage:
//
//	yarilo-admin auth scram-verifier [--iterations N] [--password X]
//
// When --password is omitted, the command reads one line from
// stdin (with prompt suppression so terminal echo can be
// disabled by the surrounding shell, e.g.
// `stty -echo; yarilo-admin auth scram-verifier; stty echo`).
func cmdAuthSCRAMVerifier(args []string) error {
	fs := flag.NewFlagSet("scram-verifier", flag.ContinueOnError)
	iterations := fs.Int("iterations", 0, "PBKDF2 iteration count (0 = library default: 600000)")
	password := fs.String("password", "", "plain password (omit to read from stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	plain := *password
	if plain == "" {
		fmt.Fprint(os.Stderr, "Password: ")
		buf, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		plain = strings.TrimRight(buf, "\r\n")
	}
	if plain == "" {
		return errors.New("empty password")
	}
	creds, err := sasl.GenerateScramSha256Credentials(plain, *iterations)
	if err != nil {
		return err
	}
	fmt.Printf("{SCRAM-SHA-256}%s\n", sasl.EncodeScramCredentials(creds))
	return nil
}

func dispatchAuthCache(args []string) error {
	if len(args) == 0 {
		printAuthCacheUsage()
		return nil
	}
	switch args[0] {
	case "flush":
		return cmdAuthCacheFlush(args[1:])
	default:
		return fmt.Errorf("unknown auth cache command %q — available: flush", args[0])
	}
}

// cmdAuthCacheFlush dials yarilo-auth's master socket and sends
// CACHE-FLUSH with the supplied user-masks. No args = full flush.
func cmdAuthCacheFlush(args []string) error {
	tlsCfg, err := buildAuthTLS()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := authclient.DialContext(ctx, authAddr, tlsCfg)
	if err != nil {
		return fmt.Errorf("dial %s: %w", authAddr, err)
	}
	defer c.Close()

	n, err := c.CacheFlush(ctx, args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Printf("%d cache entries flushed (full)\n", n)
	} else {
		fmt.Printf("%d cache entries flushed matching: %v\n", n, args)
	}
	return nil
}

// buildAuthTLS constructs an mTLS client config from the
// --auth-cert/--auth-key/--auth-ca flags. Returns nil tls.Config
// when none of them are set so dev / smoke runs over a plain TCP
// loopback work without TLS material.
func buildAuthTLS() (*tls.Config, error) {
	if authCert == "" && authKey == "" && authCA == "" {
		return nil, nil
	}
	if authCert == "" || authKey == "" || authCA == "" {
		return nil, fmt.Errorf("--auth-cert, --auth-key, --auth-ca must all be set together")
	}
	cert, err := tls.LoadX509KeyPair(authCert, authKey)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(authCA)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA %q contains no certificates", authCA)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func printAuthUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin auth <command>

Commands:
  cache flush [<user-mask> ...]  evict auth-cache entries matching the mask(s); no mask = full flush
  scram-verifier [--iterations N] [--password X]
                                 derive a {SCRAM-SHA-256} verifier blob for the SQL password column

Auth-plane flags:
  --auth-addr  yarilo-auth master socket (env: YARILO_AUTH_ADDR, default: localhost:9102)
  --auth-cert  mTLS client cert (env: YARILO_AUTH_CERT)
  --auth-key   mTLS client key  (env: YARILO_AUTH_KEY)
  --auth-ca    CA bundle that signs the server cert (env: YARILO_AUTH_CA)

When --auth-cert/--auth-key/--auth-ca are unset, the connection is plain TCP (dev / smoke only).`)
}

func printAuthCacheUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin auth cache <command>

Commands:
  flush [<user-mask> ...]  evict cache entries matching the mask(s); no mask = full flush

Mask syntax: glob with '*' (any run) and '?' (one char). Examples:
  yarilo-admin auth cache flush                     # full flush
  yarilo-admin auth cache flush 'alice@example.com' # exact user
  yarilo-admin auth cache flush 'alice@*'           # all alice's domains
  yarilo-admin auth cache flush '*@gone.example'    # whole domain`)
}
