// Package mcpclient provides a shared JSON-RPC 2.0 client for MuninnDB MCP calls.
// Used by both the store (async delivery) and inject (recall/enrichment) packages.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// requestID is a process-wide atomic counter for unique JSON-RPC request IDs.
var requestID atomic.Int64

// Client sends JSON-RPC 2.0 tools/call requests to a MuninnDB MCP endpoint.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// New creates a Client with the given endpoint, auth token, and HTTP timeout.
func New(url, token string, timeout time.Duration) *Client {
	return &Client{
		URL:   url,
		Token: token,
		HTTP:  &http.Client{Timeout: timeout},
	}
}

// ServerError is a retryable error for 5xx responses.
type ServerError struct{ Status int }

func (e *ServerError) Error() string { return fmt.Sprintf("server error: HTTP %d", e.Status) }

// ClientError is a non-retryable error for 4xx responses.
type ClientError struct{ Status int }

func (e *ClientError) Error() string { return fmt.Sprintf("client error: HTTP %d", e.Status) }

// Call sends a JSON-RPC 2.0 tools/call request and returns the raw response body.
// Returns a *ServerError for 5xx or *ClientError for 4xx status codes.
func (c *Client) Call(ctx context.Context, toolName string, args map[string]any) ([]byte, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
		"id": requestID.Add(1),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 500 {
		return nil, &ServerError{Status: resp.StatusCode}
	}
	if resp.StatusCode >= 400 {
		return nil, &ClientError{Status: resp.StatusCode}
	}

	return respBody, nil
}
