package jmap

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Headers of the login to backend contract (INTERNALS §6). JMAP has no session
// bound to a connection, so identity travels per request rather than in a
// one-time preamble.
const (
	hdrForwarded  = "Forwarded"
	hdrSessionID  = "X-Session-ID"
	hdrProxyTTL   = "X-Proxy-TTL"
	hdrUser       = "X-Yarilo-User"
	hdrForwardPfx = "X-Yarilo-Forward-"
)

// TrustMode is how this backend decides a request really came from the login
// layer. All three deny by default; none of them merge the forwarded identity
// with a locally derived one.
type TrustMode int

const (
	// TrustNone is no anchor configured: every identity request is refused.
	TrustNone TrustMode = iota
	// TrustMTLS accepts a peer holding a certificate from the internal CA.
	TrustMTLS
	// TrustNets accepts a peer whose address is inside xclient.trusted_nets.
	TrustNets
)

func (m TrustMode) String() string {
	switch m {
	case TrustMTLS:
		return "mtls"
	case TrustNets:
		return "trusted_nets"
	default:
		return "none"
	}
}

// Trust is the resolved anchor. Modes are ordered: a deployment with internal
// mTLS does not also fall back to an address list, or the weaker of the two
// would decide.
type Trust struct {
	Mode TrustMode
	Nets []*net.IPNet
}

// ResolveTrust picks the anchor from what is configured. mtls is whether the
// listener requires a client certificate, xclient whether the listener opted
// into address-based trust, and nets the configured CIDRs.
func ResolveTrust(mtls, xclient bool, nets []*net.IPNet) Trust {
	switch {
	case mtls:
		return Trust{Mode: TrustMTLS}
	case xclient && len(nets) > 0:
		return Trust{Mode: TrustNets, Nets: nets}
	default:
		return Trust{Mode: TrustNone}
	}
}

// allows reports whether this request's peer may speak for a user.
func (t Trust) allows(r *http.Request) bool {
	switch t.Mode {
	case TrustMTLS:
		// The handshake already rejected a certificate the internal CA did not
		// sign; this only confirms one was presented at all.
		return r.TLS != nil && len(r.TLS.PeerCertificates) > 0
	case TrustNets:
		ip := net.ParseIP(hostOnly(r.RemoteAddr))
		if ip == nil {
			return false
		}
		for _, n := range t.Nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// identity is what the login layer asserted about one request.
type identity struct {
	user      string
	clientIP  string
	sessionID string
	ttl       int
	// forward carries the passdb forward_ fields, one header each.
	forward map[string]string
}

// readIdentity parses the contract headers. It is called only after the trust
// gate, so the values are taken at face value here by design.
func readIdentity(r *http.Request) (identity, error) {
	id := identity{
		user:      strings.TrimSpace(r.Header.Get(hdrUser)),
		sessionID: strings.TrimSpace(r.Header.Get(hdrSessionID)),
		ttl:       -1,
	}
	if id.user == "" {
		return id, fmt.Errorf("jmap: no %s header", hdrUser)
	}
	if v := r.Header.Get(hdrProxyTTL); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return id, fmt.Errorf("jmap: bad %s %q: %w", hdrProxyTTL, v, err)
		}
		id.ttl = n
	}
	id.clientIP = forwardedFor(r.Header.Get(hdrForwarded))
	for name, vals := range r.Header {
		if !strings.HasPrefix(name, hdrForwardPfx) || len(vals) == 0 {
			continue
		}
		key := strings.ToLower(strings.TrimPrefix(name, hdrForwardPfx))
		v, err := url.QueryUnescape(vals[0])
		if err != nil {
			return id, fmt.Errorf("jmap: bad %s%s value: %w", hdrForwardPfx, key, err)
		}
		if id.forward == nil {
			id.forward = make(map[string]string)
		}
		id.forward[key] = v
	}
	return id, nil
}

// forwardedFor extracts the client address from RFC 7239. It is the only source
// of client origin: X-Forwarded-For is deliberately not read, so one value
// cannot arrive two ways and disagree.
func forwardedFor(h string) string {
	// Only the first element matters; later ones describe hops further out.
	if i := strings.IndexByte(h, ','); i >= 0 {
		h = h[:i]
	}
	for _, part := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "for") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if host, _, err := net.SplitHostPort(v); err == nil {
			return strings.Trim(host, "[]")
		}
		return strings.Trim(v, "[]")
	}
	return ""
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
