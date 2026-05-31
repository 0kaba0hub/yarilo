package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func apiGet(path string) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodGet, path, nil)
}
func apiPost(path string, body any) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodPost, path, body)
}
func apiPatch(path string, body any) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodPatch, path, body)
}
func apiDelete(path string) ([]byte, error) {
	return doRequest(apiURL, apiToken, http.MethodDelete, path, nil)
}

// adminAPIGet / adminAPIPost talk to the yarilo-admin-api endpoint
// (dict / acl / quota / folder / user operations). Director endpoint
// retains the apiURL/apiToken pair for director-specific ops.
func adminAPIGet(path string) ([]byte, error) {
	return doRequest(adminAPIURL, adminAPIToken, http.MethodGet, path, nil)
}

func adminAPIPost(path string, body any) ([]byte, error) {
	return doRequest(adminAPIURL, adminAPIToken, http.MethodPost, path, body)
}

// adminAPIStream sends a POST and returns the raw response body as
// an io.ReadCloser so streaming endpoints (NDJSON iterate) can be
// consumed line-by-line. Caller MUST Close the body.
func adminAPIStream(path string, body any) (io.ReadCloser, error) {
	resp, err := doRawRequest(adminAPIURL, adminAPIToken, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return resp.Body, nil
}

func doRequest(baseURL, token, method, path string, body any) ([]byte, error) {
	resp, err := doRawRequest(baseURL, token, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func doRawRequest(baseURL, token, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, strings.TrimRight(baseURL, "/")+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func printJSON(data []byte, err error) error {
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if jsonErr := json.Indent(&buf, data, "", "  "); jsonErr != nil {
		fmt.Println(string(data))
		return nil
	}
	fmt.Println(buf.String())
	return nil
}
