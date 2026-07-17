package main

import (
	"net/http"
	"testing"
)

func TestAPIError(t *testing.T) {
	resp := &http.Response{Status: "400 Bad Request", StatusCode: 400}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"json error envelope", `{"error":"not a configured quota_clone backend: metadata"}`, "not a configured quota_clone backend: metadata"},
		{"json without error field", `{"note":"x"}`, `HTTP 400 Bad Request: {"note":"x"}`},
		{"plain text body", "boom", "HTTP 400 Bad Request: boom"},
		{"empty body", "", "HTTP 400 Bad Request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := apiError(resp, []byte(c.body)).Error(); got != c.want {
				t.Errorf("apiError = %q, want %q", got, c.want)
			}
		})
	}
}
