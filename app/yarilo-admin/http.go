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

func apiGet(path string) ([]byte, error)             { return doRequest(http.MethodGet, path, nil) }
func apiPost(path string, body any) ([]byte, error)  { return doRequest(http.MethodPost, path, body) }
func apiPatch(path string, body any) ([]byte, error) { return doRequest(http.MethodPatch, path, body) }
func apiDelete(path string) ([]byte, error)          { return doRequest(http.MethodDelete, path, nil) }

func doRequest(method, path string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, strings.TrimRight(apiURL, "/")+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
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
