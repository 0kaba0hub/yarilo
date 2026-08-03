package jmaplogin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// corsMaxAge is how long a browser may cache a preflight result.
const corsMaxAge = 24 * time.Hour

// corsAllowedHeaders is what a JMAP client sends: its credentials and a JSON
// content type. Listing them rather than reflecting the request keeps the
// preflight answer independent of what the caller asks for.
var corsAllowedHeaders = []string{"Authorization", "Content-Type", "Accept"}

// corsAllowedMethods covers the session resource, the API endpoint and the
// blob endpoints that arrive with the backend.
var corsAllowedMethods = []string{http.MethodGet, http.MethodPost, http.MethodOptions}

// cors decides whether a browser origin may call this endpoint. An empty
// allow-list denies every cross-origin request: an endpoint any page can call
// with the user's credentials is an account-takeover surface, so it is opt-in.
type cors struct {
	origins  []string
	wildcard bool
}

func newCORS(allowed []string) cors {
	c := cors{}
	for _, o := range allowed {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "*" {
			c.wildcard = true
			continue
		}
		c.origins = append(c.origins, strings.TrimRight(o, "/"))
	}
	return c
}

// allows reports whether origin is permitted. Matching is exact: a JMAP
// endpoint is not a public API, and prefix or suffix matching is how an
// allow-list quietly becomes a wildcard.
func (c cors) allows(origin string) bool {
	if origin == "" {
		return false
	}
	if c.wildcard {
		return true
	}
	origin = strings.TrimRight(origin, "/")
	for _, o := range c.origins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// apply writes the response headers for an allowed origin. Credentials are only
// offered to a named origin: the pair "*" plus credentials is rejected by every
// browser, so advertising it would be a lie.
func (c cors) apply(w http.ResponseWriter, origin string) {
	h := w.Header()
	h.Add("Vary", "Origin")
	if c.wildcard && len(c.origins) == 0 {
		h.Set("Access-Control-Allow-Origin", "*")
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
}

// preflight answers an OPTIONS probe. A denied origin gets no CORS headers at
// all rather than an error, which is what the browser expects to see.
func (c cors) preflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !c.allows(origin) {
		w.Header().Add("Vary", "Origin")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	c.apply(w, origin)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", strings.Join(corsAllowedMethods, ", "))
	h.Set("Access-Control-Allow-Headers", strings.Join(corsAllowedHeaders, ", "))
	h.Set("Access-Control-Max-Age", strconv.Itoa(int(corsMaxAge.Seconds())))
	w.WriteHeader(http.StatusNoContent)
}
