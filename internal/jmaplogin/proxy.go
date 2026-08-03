package jmaplogin

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// proxyTTL is the initial X-Proxy-TTL, decremented per hop. Matches
// LOGIN_PROXY_TTL used by the byte-pipe protocols.
const proxyTTL = 5

// Headers of the login to backend contract. Identity travels in them because
// there is no session bound to the connection to hand over.
const (
	hdrForwarded = "Forwarded"
	hdrSessionID = "X-Session-ID"
	hdrProxyTTL  = "X-Proxy-TTL"
	hdrUser      = "X-Yarilo-User"
)

// hopHeaders are stripped from the client's request before proxying: a client
// must not be able to name itself, and the trust rule on the backend is the
// second line of defence, not the first.
var hopHeaders = []string{hdrForwarded, hdrSessionID, hdrProxyTTL, hdrUser,
	"X-Forwarded-For", "X-Forwarded-Port", "X-Forwarded-Proto"}

// backendProxy forwards one request to the pod that owns the user's state.
type backendProxy struct {
	scheme    string
	localIP   string
	transport http.RoundTripper
}

func newBackendProxy(opts Options) *backendProxy {
	scheme := "http"
	tr := &http.Transport{
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	if opts.BackendTLS != nil {
		scheme = "https"
		tr.TLSClientConfig = opts.BackendTLS
	}
	return &backendProxy{scheme: scheme, localIP: opts.LocalIP, transport: tr}
}

func (p *backendProxy) serve(w http.ResponseWriter, r *http.Request, backend, username, sessionID, clientIP string) {
	target := &url.URL{Scheme: p.scheme, Host: backend}
	rp := &httputil.ReverseProxy{
		Transport: p.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.Host = target.Host
			for _, h := range hopHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set(hdrForwarded, forwardedValue(r, clientIP, p.localIP))
			pr.Out.Header.Set(hdrSessionID, sessionID)
			pr.Out.Header.Set(hdrProxyTTL, fmt.Sprint(nextTTL(r)))
			pr.Out.Header.Set(hdrUser, username)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("jmap-login: backend unreachable", "backend", backend, "user", username, "err", err)
			writeProblem(w, http.StatusBadGateway, "Backend unavailable")
		},
	}
	rp.ServeHTTP(w, r)
}

// forwardedValue renders RFC 7239. It is the HTTP equivalent of XCLIENT and the
// only client-origin header the backend reads, so it carries the address, the
// port and whether the client's leg was TLS.
func forwardedValue(r *http.Request, clientIP, localIP string) string {
	var b strings.Builder
	b.WriteString(`for="`)
	b.WriteString(net.JoinHostPort(clientIP, clientPort(r)))
	b.WriteString(`"`)
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	b.WriteString(";proto=" + proto)
	if localIP != "" {
		b.WriteString(`;by="` + localIP + `"`)
	}
	return b.String()
}

func clientPort(r *http.Request) string {
	_, port, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "0"
	}
	return port
}

// nextTTL decrements the incoming hop budget, or starts it when this is the
// first proxy. A request arriving at zero is rejected before it gets here.
func nextTTL(r *http.Request) int {
	n := proxyTTL
	if v := r.Header.Get(hdrProxyTTL); v != "" {
		if parsed, err := parseTTL(v); err == nil {
			n = parsed
		}
	}
	if n > 0 {
		n--
	}
	return n
}

func parseTTL(v string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, err
	}
	if n < 0 || n > proxyTTL {
		return 0, fmt.Errorf("jmap-login: ttl %d out of range", n)
	}
	return n, nil
}
