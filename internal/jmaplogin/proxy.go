package jmaplogin

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// firstHopTTL is the X-Proxy-TTL this proxy always emits. The login layer is
// by definition the first hop, so the budget starts at LOGIN_PROXY_TTL minus
// this one; the client's own value is never read, or a caller could widen its
// own loop budget.
const firstHopTTL = "4"

// Headers of the login to backend contract. Identity travels in them because
// there is no session bound to the connection to hand over.
const (
	hdrForwarded = "Forwarded"
	hdrSessionID = "X-Session-ID"
	hdrProxyTTL  = "X-Proxy-TTL"
	hdrUser      = "X-Yarilo-User"
)

// yariloPrefix covers every header the backend treats as coming from this
// proxy, including the open-ended X-Yarilo-Forward-<key> family. Stripping by
// prefix rather than by list means a header added to the contract later cannot
// be forgotten here and become a way in.
const yariloPrefix = "X-Yarilo-"

// hopHeaders are the fixed names stripped from the client's request before
// proxying. A client must not be able to name itself; the backend's trust rule
// is the second line of defence, not the first.
var hopHeaders = []string{hdrForwarded, hdrSessionID, hdrProxyTTL,
	"X-Forwarded-For", "X-Forwarded-Port", "X-Forwarded-Proto"}

// stripClientIdentity removes everything the backend would otherwise read as
// this proxy's word.
func stripClientIdentity(h http.Header) {
	for _, name := range hopHeaders {
		h.Del(name)
	}
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), yariloPrefix) {
			h.Del(name)
		}
	}
}

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
			stripClientIdentity(pr.Out.Header)
			pr.Out.Header.Set(hdrForwarded, forwardedValue(r, clientIP, p.localIP))
			pr.Out.Header.Set(hdrSessionID, sessionID)
			pr.Out.Header.Set(hdrProxyTTL, firstHopTTL)
			pr.Out.Header.Set(hdrUser, username)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("jmap-login: backend unreachable", "backend", backend, "user", username, "err", err)
			jmapcore.WriteProblem(w, http.StatusBadGateway, "Backend unavailable")
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
