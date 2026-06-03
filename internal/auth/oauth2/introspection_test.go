package oauth2

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// introspectionServer captures the inbound request shape so each
// test can assert the validator built it correctly.
type introspectionServer struct {
	*httptest.Server
	method     string
	path       string
	query      string
	header     http.Header
	body       string
	authzUser  string
	authzPass  string
	hadBasic   bool
	respStatus int
	respBody   string
}

func newIntrospectionServer(t *testing.T, status int, body string) *introspectionServer {
	t.Helper()
	srv := &introspectionServer{respStatus: status, respBody: body}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.method = r.Method
		srv.path = r.URL.Path
		srv.query = r.URL.RawQuery
		srv.header = r.Header.Clone()
		srv.authzUser, srv.authzPass, srv.hadBasic = r.BasicAuth()
		buf, _ := io.ReadAll(r.Body)
		srv.body = string(buf)
		if srv.respStatus != 0 {
			w.WriteHeader(srv.respStatus)
		}
		if srv.respBody != "" {
			w.Write([]byte(srv.respBody)) //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Server.Close)
	return srv
}

// TestIntrospection_PostFormDefault — default mode is RFC 7662
// POST form. Token in body, content-type set correctly.
func TestIntrospection_PostFormDefault(t *testing.T) {
	srv := newIntrospectionServer(t, 200, `{"active":true,"email":"alice@example.com"}`)
	v, _ := NewIntrospectionValidator(IntrospectionConfig{URL: srv.URL})
	c, err := v.Validate(context.Background(), "tkn")
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "alice@example.com" {
		t.Errorf("username = %q", c.Username)
	}
	if srv.method != "POST" {
		t.Errorf("method = %s, want POST", srv.method)
	}
	if !strings.Contains(srv.body, "token=tkn") {
		t.Errorf("body should carry token=tkn, got %q", srv.body)
	}
	if srv.header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", srv.header.Get("Content-Type"))
	}
}

// TestIntrospection_AuthBearer — mode=auth sends Authorization
// header instead of form body.
func TestIntrospection_AuthBearer(t *testing.T) {
	srv := newIntrospectionServer(t, 200, `{"active":true,"email":"x"}`)
	v, _ := NewIntrospectionValidator(IntrospectionConfig{
		URL: srv.URL, Mode: IntrospectionAuthBearer,
	})
	if _, err := v.Validate(context.Background(), "tkn"); err != nil {
		t.Fatal(err)
	}
	if srv.header.Get("Authorization") != "Bearer tkn" {
		t.Errorf("auth header = %q", srv.header.Get("Authorization"))
	}
	if srv.body != "" {
		t.Errorf("body should be empty, got %q", srv.body)
	}
}

// TestIntrospection_Get — mode=get carries token in query string.
func TestIntrospection_Get(t *testing.T) {
	srv := newIntrospectionServer(t, 200, `{"active":true,"email":"x"}`)
	v, _ := NewIntrospectionValidator(IntrospectionConfig{
		URL: srv.URL, Mode: IntrospectionGet,
	})
	if _, err := v.Validate(context.Background(), "tkn"); err != nil {
		t.Fatal(err)
	}
	if srv.method != "GET" {
		t.Errorf("method = %s", srv.method)
	}
	if !strings.Contains(srv.query, "token=tkn") {
		t.Errorf("query should carry token=tkn, got %q", srv.query)
	}
}

// TestIntrospection_ClientCreds — when ClientID is set, Basic auth
// header carries the introspection-endpoint credentials.
func TestIntrospection_ClientCreds(t *testing.T) {
	srv := newIntrospectionServer(t, 200, `{"active":true,"email":"x"}`)
	v, _ := NewIntrospectionValidator(IntrospectionConfig{
		URL: srv.URL, ClientID: "my-client", ClientSecret: "shh",
	})
	if _, err := v.Validate(context.Background(), "tkn"); err != nil {
		t.Fatal(err)
	}
	if !srv.hadBasic || srv.authzUser != "my-client" || srv.authzPass != "shh" {
		t.Errorf("basic auth wrong: had=%v user=%q", srv.hadBasic, srv.authzUser)
	}
}

// TestIntrospection_InactiveRejected — active=false maps to
// ErrTokenInactive.
func TestIntrospection_InactiveRejected(t *testing.T) {
	srv := newIntrospectionServer(t, 200, `{"active":false}`)
	v, _ := NewIntrospectionValidator(IntrospectionConfig{URL: srv.URL})
	_, err := v.Validate(context.Background(), "tkn")
	if !errors.Is(err, ErrTokenInactive) {
		t.Errorf("err = %v, want wrap of ErrTokenInactive", err)
	}
}

// TestIntrospection_MissingActiveTreatedAsInactive — defensive:
// a misbehaving IdP that omits "active" should not accidentally
// grant access.
func TestIntrospection_MissingActiveTreatedAsInactive(t *testing.T) {
	srv := newIntrospectionServer(t, 200, `{"email":"x"}`)
	v, _ := NewIntrospectionValidator(IntrospectionConfig{URL: srv.URL})
	_, err := v.Validate(context.Background(), "tkn")
	if !errors.Is(err, ErrTokenInactive) {
		t.Errorf("err = %v, want ErrTokenInactive", err)
	}
}

// TestIntrospection_ExpiredRejected — exp in past beyond grace.
func TestIntrospection_ExpiredRejected(t *testing.T) {
	resp := map[string]interface{}{
		"active": true,
		"email":  "x@y",
		"exp":    time.Now().Add(-time.Hour).Unix(),
	}
	body, _ := json.Marshal(resp)
	srv := newIntrospectionServer(t, 200, string(body))
	v, _ := NewIntrospectionValidator(IntrospectionConfig{
		URL: srv.URL, ExpireGrace: time.Second,
	})
	_, err := v.Validate(context.Background(), "tkn")
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v", err)
	}
}

// TestIntrospection_HTTPNon2xx → ErrUpstream.
func TestIntrospection_HTTPNon2xx(t *testing.T) {
	srv := newIntrospectionServer(t, 500, "boom")
	v, _ := NewIntrospectionValidator(IntrospectionConfig{URL: srv.URL})
	_, err := v.Validate(context.Background(), "tkn")
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}

// TestIntrospection_BadJSON → ErrUpstream.
func TestIntrospection_BadJSON(t *testing.T) {
	srv := newIntrospectionServer(t, 200, "not json")
	v, _ := NewIntrospectionValidator(IntrospectionConfig{URL: srv.URL})
	_, err := v.Validate(context.Background(), "tkn")
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v", err)
	}
}

// TestNewIntrospectionValidator_Validation — invalid configs
// rejected at construction.
func TestNewIntrospectionValidator_Validation(t *testing.T) {
	if _, err := NewIntrospectionValidator(IntrospectionConfig{}); err == nil {
		t.Error("empty URL accepted")
	}
	if _, err := NewIntrospectionValidator(IntrospectionConfig{URL: "x", Mode: "weird"}); err == nil {
		t.Error("invalid mode accepted")
	}
}

// TestTokeninfo_HappyPath — Google-style ?access_token= query
// with active=200-implied response.
func TestTokeninfo_HappyPath(t *testing.T) {
	srv := newIntrospectionServer(t, 200, `{"email":"alice@example.com","scope":"openid email"}`)
	v, _ := NewTokeninfoValidator(TokeninfoConfig{URL: srv.URL})
	c, err := v.Validate(context.Background(), "tkn")
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "alice@example.com" {
		t.Errorf("username = %q", c.Username)
	}
	if !c.Active {
		t.Errorf("active should be true on 2xx")
	}
	if !strings.Contains(srv.query, "access_token=tkn") {
		t.Errorf("query should carry access_token=tkn, got %q", srv.query)
	}
}

// TestTokeninfo_4xxInactive — 400 / 401 = token revoked/unknown.
func TestTokeninfo_4xxInactive(t *testing.T) {
	srv := newIntrospectionServer(t, 401, "bad token")
	v, _ := NewTokeninfoValidator(TokeninfoConfig{URL: srv.URL})
	_, err := v.Validate(context.Background(), "tkn")
	if !errors.Is(err, ErrTokenInactive) {
		t.Errorf("err = %v, want ErrTokenInactive", err)
	}
}

// TestTokeninfo_5xxUpstream — 5xx = transient.
func TestTokeninfo_5xxUpstream(t *testing.T) {
	srv := newIntrospectionServer(t, 503, "")
	v, _ := NewTokeninfoValidator(TokeninfoConfig{URL: srv.URL})
	_, err := v.Validate(context.Background(), "tkn")
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}
