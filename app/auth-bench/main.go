// auth-bench times what a dial to the yarilo-auth master costs against the
// lookup it carries -- the ratio the connection pool decision turned on
// (#1402) and the way to confirm the pool in the field afterwards.
//
// Not built into the image: it belongs to whoever is measuring, and shipping a
// prober in a mail server is a liability nobody asked for. Build it into a
// scratch pod with the internal-tls secret mounted.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	addr := flag.String("addr", "yarilo-auth:9102", "auth master address")
	user := flag.String("user", "u1@d00001.test", "username to resolve")
	iters := flag.Int("iters", 100, "iterations per phase")
	cert := flag.String("cert", "/etc/yarilo/internal-tls/tls.crt", "")
	key := flag.String("key", "/etc/yarilo/internal-tls/tls.key", "")
	ca := flag.String("ca", "/etc/yarilo/internal-tls/ca.crt", "")
	name := flag.String("server-name", "yarilo-internal", "")
	cache := flag.Int("session-cache", 0, "TLS session cache size; >0 enables resumption between dials")
	tcpOnly := flag.Bool("tcp-only", false, "time a bare TCP connect to the same port, to split TCP from TLS")
	flag.Parse()

	if *tcpOnly {
		ds := measure(*iters, func() error {
			c, err := net.DialTimeout("tcp", *addr, 5*time.Second)
			if err != nil {
				return err
			}
			return c.Close()
		})
		fmt.Printf("bare TCP connect p50 %.2f ms  p95 %.2f ms\n", ms(pct(ds, 50)), ms(pct(ds, 95)))
		return
	}

	// Same construction the resolvers use, session cache included (0/0 in the
	// sandbox, i.e. no resumption): a probe that resumed sessions the real
	// callers cannot would measure a handshake nobody pays.
	ttl := 0
	if *cache > 0 {
		ttl = 3600
	}
	cfg, err := mtls.ClientConfig(*cert, *key, *ca, *name, *cache, ttl)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth-bench: tls:", err)
		os.Exit(1)
	}

	var resumed int
	perRequest := measure(*iters, func() error { return dialLookupClose(*addr, cfg, *user, &resumed) })

	c, err := authclient.Dial(*addr, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth-bench: dial:", err)
		os.Exit(1)
	}
	defer c.Close()
	pooled := measure(*iters, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := c.Userdb(ctx, *user)
		return err
	})

	hs50 := pct(perRequest, 50) - pct(pooled, 50)
	fmt.Printf(`auth resolution cost (%d iterations)
  dial + handshake + USER + close   p50 %.2f ms   p95 %.2f ms
  USER on a held connection         p50 %.2f ms   p95 %.2f ms
  the dial adds                     %.2f ms  =  %.0f%% of the lookup it carries
  TLS sessions resumed              %d of %d
`, *iters,
		ms(pct(perRequest, 50)), ms(pct(perRequest, 95)),
		ms(pct(pooled, 50)), ms(pct(pooled, 95)),
		ms(hs50), 100*float64(hs50)/float64(pct(pooled, 50)), resumed, *iters)
}

// dialLookupClose reproduces exactly what a resolver does per request -- dial,
// consume the greeting, one USER, close -- rather than calling authclient, so
// the probe can report whether the TLS session resumed. The alternative was a
// TLSState accessor on the client, and a production API added for a scratch
// measurement is a worse trade.
func dialLookupClose(addr string, cfg *tls.Config, user string, resumed *int) error {
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	rd := bufio.NewReader(c)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "DONE" {
			break
		}
	}
	if c.ConnectionState().DidResume {
		*resumed++
	}
	if _, err := fmt.Fprintf(c, "USER\t1\t%s\n", user); err != nil {
		return err
	}
	_, err = rd.ReadString('\n')
	return err
}

func measure(iters int, fn func() error) []time.Duration {
	ds := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		t0 := time.Now()
		if err := fn(); err != nil {
			fmt.Fprintln(os.Stderr, "auth-bench:", err)
			os.Exit(1)
		}
		ds = append(ds, time.Since(t0))
	}
	return ds
}

func pct(ds []time.Duration, p int) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := (len(s) * p) / 100
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
